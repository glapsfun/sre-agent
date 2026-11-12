package timeline

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/glapsfun/sre-agent/internal/executil"
	"github.com/glapsfun/sre-agent/internal/safety"
)

// ProducerError retains Stream's already-redacted diagnostics and normal
// nonzero exit status for source classification, without retaining raw stdout.
// Stderr remains untrusted text and needs the output boundary used by its caller.
type ProducerError struct {
	Stderr   string
	ExitCode *int
	cause    error
}

func (e *ProducerError) Error() string {
	message := fmt.Sprintf("read Kubernetes list: %v", e.cause)
	if diagnostic := strings.TrimSpace(e.Stderr); diagnostic != "" {
		message += ": " + diagnostic
	}
	return message
}

func (e *ProducerError) Unwrap() error { return e.cause }

// ReadList visits bounded raw records without bounding the number of records.
// Callers must discard their source batch on error and project safe fields before
// retaining data. The visitor must return promptly, including on cancellation.
func ReadList(ctx context.Context, kube KubeRunner, resource string, args []string, visit func(json.RawMessage) error) error {
	if kube == nil || visit == nil {
		return errors.New("Kubernetes runner and list visitor are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	producerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	// Cancellation also releases a reader waiting for an idle producer.
	stop := context.AfterFunc(producerCtx, func() { _ = reader.CloseWithError(producerCtx.Err()) })
	defer stop()
	done := make(chan executil.Result, 1)
	command := append([]string{"get", resource}, args...)
	command = append(command, "--output=json")
	go func() {
		result := kube.Stream(producerCtx, writer, command...)
		_ = writer.CloseWithError(result.Err)
		done <- result
	}()
	parseErr := (&listReader{ctx: ctx, input: bufio.NewReader(reader)}).read(visit)
	_ = reader.Close()
	if parseErr != nil {
		cancel()
	}
	result := <-done
	if err := ctx.Err(); err != nil {
		return err
	}
	// Cleanup failures must not replace the consumer error that caused them.
	if result.Err != nil && !(parseErr != nil && (errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, io.ErrClosedPipe))) {
		return &ProducerError{Stderr: result.Stderr, ExitCode: result.ExitCode, cause: result.Err}
	}
	return parseErr
}

var errList = errors.New("invalid or incomplete Kubernetes list JSON")

type listReader struct {
	ctx   context.Context
	input *bufio.Reader
}

func (r *listReader) peek() (byte, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	b, err := r.input.Peek(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *listReader) space() error {
	for {
		b, err := r.peek()
		if err != nil {
			return err
		}
		if !strings.ContainsRune(" \r\n\t", rune(b)) {
			return nil
		}
		_, _ = r.input.ReadByte()
	}
}

func (r *listReader) expect(want byte) error {
	if err := r.space(); err != nil {
		return errList
	}
	b, _ := r.input.ReadByte()
	if b != want {
		return errList
	}
	return nil
}

// Frame one value before decoding so json.Decoder cannot allocate an unbounded
// string or member. Syntax validation remains with the shared bounded decoder.
func (r *listReader) value() (json.RawMessage, error) {
	if err := r.space(); err != nil {
		return nil, errList
	}
	var raw []byte
	depth, structure := 0, 0
	quoted, escaped := false, false
	for {
		b, err := r.peek()
		if err != nil {
			if err == io.EOF && len(raw) > 0 && !quoted && depth == 0 {
				break
			}
			return nil, errList
		}
		if len(raw) > 0 && !quoted && depth == 0 && strings.ContainsRune(" \r\n\t,]}", rune(b)) {
			break
		}
		if len(raw) >= safety.MaxResponseBytes {
			return nil, safety.ErrResponseTooLarge
		}
		_, _ = r.input.ReadByte()
		raw = append(raw, b)
		if quoted {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				quoted = false
				if depth == 0 {
					break
				}
			}
			continue
		}
		switch b {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			structure++
		case '}', ']':
			depth--
			structure++
		case ',', ':':
			structure++
		}
		if structure > safety.MaxJSONStructure {
			return nil, safety.ErrResponseComplexity
		}
		if depth < 0 {
			return nil, errList
		}
		if depth == 0 && (b == '}' || b == ']') {
			break
		}
	}
	var validated json.RawMessage
	if err := safety.DecodeJSON(r.ctx, string(raw), &validated); err != nil {
		if r.ctx.Err() != nil {
			return nil, r.ctx.Err()
		}
		if errors.Is(err, safety.ErrResponseComplexity) {
			return nil, err
		}
		return nil, errList
	}
	return validated, nil
}

func (r *listReader) items(visit func(json.RawMessage) error) error {
	if err := r.expect('['); err != nil {
		return err
	}
	if err := r.space(); err != nil {
		return errList
	}
	if b, _ := r.peek(); b == ']' {
		_, _ = r.input.ReadByte()
		return nil
	}
	for {
		raw, err := r.value()
		if err != nil {
			return err
		}
		if err = visit(raw); err != nil {
			return err
		}
		if err = r.space(); err != nil {
			return errList
		}
		b, _ := r.input.ReadByte()
		if b == ']' {
			return nil
		}
		if b != ',' {
			return errList
		}
	}
}

func (r *listReader) read(visit func(json.RawMessage) error) error {
	if err := r.expect('{'); err != nil {
		return err
	}
	seen := map[string]bool{}
	var kind, apiVersion string
	envelopeBytes := 0
	for {
		keyRaw, err := r.value()
		if err != nil {
			return err
		}
		envelopeBytes += len(keyRaw)
		if envelopeBytes > safety.MaxResponseBytes {
			return safety.ErrResponseTooLarge
		}
		var key string
		if len(keyRaw) == 0 || keyRaw[0] != '"' || json.Unmarshal(keyRaw, &key) != nil || seen[key] {
			return errList
		}
		seen[key] = true
		if len(seen) > safety.MaxJSONStructure {
			return safety.ErrResponseComplexity
		}
		if err = r.expect(':'); err != nil {
			return err
		}
		if key == "items" {
			if err = r.items(visit); err != nil {
				return err
			}
		} else {
			raw, err := r.value()
			if err != nil {
				return err
			}
			envelopeBytes += len(raw)
			if envelopeBytes > safety.MaxResponseBytes {
				return safety.ErrResponseTooLarge
			}
			switch key {
			case "kind":
				if json.Unmarshal(raw, &kind) != nil {
					return errList
				}
			case "apiVersion":
				if json.Unmarshal(raw, &apiVersion) != nil {
					return errList
				}
			case "metadata":
				var metadata struct {
					Continue string `json:"continue"`
				}
				if json.Unmarshal(raw, &metadata) != nil || metadata.Continue != "" {
					return errList
				}
			}
		}
		if err = r.space(); err != nil {
			return errList
		}
		b, _ := r.input.ReadByte()
		if b == '}' {
			break
		}
		if b != ',' {
			return errList
		}
	}
	if !seen["items"] || apiVersion == "" || !strings.HasSuffix(kind, "List") {
		return errList
	}
	if err := r.space(); err != io.EOF {
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		return errList
	}
	return nil
}
