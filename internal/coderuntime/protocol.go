package coderuntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sync"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/collect"
)

var errProtocol = errors.New("invalid runtime protocol")
var errRuntimeLimit = errors.New("runtime protocol limit exceeded")
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type childFrame struct {
	Type   string
	ID     string
	Key    string
	Values []collect.Value
}

func validJSON(data []byte) error {
	if !utf8.Valid(data) {
		return errProtocol
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		tok, err := d.Token()
		if err != nil {
			return errProtocol
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		if depth >= 64 {
			return errProtocol
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return errProtocol
				}
				s, ok := key.(string)
				if !ok || seen[s] {
					return errProtocol
				}
				seen[s] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errProtocol
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return errProtocol
	}
	if _, err := d.Token(); err != io.EOF {
		return errProtocol
	}
	return nil
}

func parseFrame(line []byte) (childFrame, error) {
	var f childFrame
	if len(line) > 65536+128 {
		return f, errRuntimeLimit
	}
	if err := validJSON(line); err != nil {
		return f, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &fields) != nil || json.Unmarshal(fields["type"], &f.Type) != nil {
		return f, errProtocol
	}
	switch f.Type {
	case "ready":
		if len(fields) != 1 || len(line) > 4096 {
			return f, errProtocol
		}
	case "call":
		if len(fields) != 3 || len(line) > 4096 || json.Unmarshal(fields["id"], &f.ID) != nil || json.Unmarshal(fields["key"], &f.Key) != nil || !idPattern.MatchString(f.ID) || f.Key == "" {
			return f, errProtocol
		}
	case "final":
		raw := fields["values"]
		if len(fields) != 2 || len(raw) == 0 || raw[0] != '[' || len(raw) > 65536 {
			return f, errProtocol
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&f.Values) != nil {
			return f, errProtocol
		}
		var items []map[string]json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return f, errProtocol
		}
		for _, item := range items {
			if len(item) != 3 || item["key"] == nil || item["source_id"] == nil || item["data"] == nil {
				return f, errProtocol
			}
		}
	default:
		return f, errProtocol
	}
	return f, nil
}

type protocolBudget struct {
	mu                          sync.Mutex
	frames, child, host, stderr int
}

func (b *protocolBudget) consume(kind string, n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch kind {
	case "child":
		if b.frames+1 > 128 || b.child+n > 256<<10 {
			return errRuntimeLimit
		}
		b.frames++
		b.child += n
	case "host":
		if b.host+n > 32<<20 {
			return errRuntimeLimit
		}
		b.host += n
	case "stderr":
		if b.stderr+n > 64<<10 {
			return errRuntimeLimit
		}
		b.stderr += n
	}
	return nil
}

type frameRead struct {
	frame childFrame
	err   error
}

func readFrames(ctx context.Context, r io.Reader, b *protocolBudget, out chan<- frameRead, abort func()) {
	reader := bufio.NewReaderSize(r, 65536+129)
	for {
		line, err := reader.ReadSlice('\n')
		if len(line) > 0 {
			if e := b.consume("child", len(line)); e != nil {
				err = e
			}
			if len(line) > 65536+129 {
				err = errRuntimeLimit
			}
			if err == nil {
				var f childFrame
				f, err = parseFrame(line[:len(line)-1])
				if err != nil {
					abort()
				}
				select {
				case out <- frameRead{f, err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
				continue
			}
			if err == io.EOF {
				err = errProtocol
			}
		}
		if err != nil && err != io.EOF {
			abort()
		}
		select {
		case out <- frameRead{err: err}:
		case <-ctx.Done():
		}
		return
	}
}

type writeJob struct {
	data []byte
	id   string
}
type callState uint8

const (
	callbackPending callState = iota
	replyQueued
	replyDelivering
)

type pendingCalls struct {
	mu  sync.Mutex
	ids map[string]callState
}

// The empty ID tracks initial invocation delivery without using a helper slot.
func (p *pendingCalls) reserve(id string) (accepted, waiting bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, delivering := 0, false
	for id, state := range p.ids {
		if id != "" {
			n++
			delivering = delivering || state == replyDelivering
		}
	}
	if n >= 2 {
		return false, delivering
	}
	p.ids[id] = callbackPending
	return true, false
}

func (p *pendingCalls) finalReady() (ready, invalid bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, state := range p.ids {
		if state != replyDelivering {
			return false, true
		}
	}
	return len(p.ids) == 0, false
}

// A child can consume a reply before Write returns. Record that ambiguous
// delivery separately so the event loop can still reject other pending work.
func protocolWriter(ctx context.Context, w io.Writer, b *protocolBudget, pending *pendingCalls, jobs <-chan writeJob, done chan<- error) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-jobs:
			if ctx.Err() != nil {
				return
			}
			pending.mu.Lock()
			pending.ids[job.id] = replyDelivering
			pending.mu.Unlock()
			err := b.consume("host", len(job.data))
			if err == nil {
				var n int
				n, err = w.Write(job.data)
				if err == nil && n != len(job.data) {
					err = io.ErrShortWrite
				}
			}
			pending.mu.Lock()
			if err == nil {
				delete(pending.ids, job.id)
			}
			pending.mu.Unlock()
			select {
			case done <- err:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}
}

func runProtocol(ctx context.Context, p *sandboxProcess, b *protocolBudget, inv collect.Invocation, helper collect.Helper, verify func(context.Context) error, proxyFailure <-chan struct{}, finish func() error) ([]collect.Value, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			p.Abort()
		case <-proxyFailure:
			p.Abort()
		case <-stopped:
		}
	}()
	defer close(stopped)
	frames := make(chan frameRead)
	go readFrames(ctx, p.Stdout, b, frames, p.Abort)
	stderr := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := p.Stderr.Read(buf)
			if n > 0 {
				if e := b.consume("stderr", n); e != nil {
					p.Abort()
					stderr <- e
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				stderr <- err
				return
			}
		}
	}()
	pending := &pendingCalls{ids: map[string]callState{}}
	jobs := make(chan writeJob, 2)
	written := make(chan error, 2)
	go protocolWriter(ctx, p.Stdin, b, pending, jobs, written)
	helperErrors := make(chan error, 2)
	seen := map[string]bool{}
	allowed := map[string]bool{}
	for _, key := range inv.Keys {
		allowed[key.Key] = true
	}
	startCall := func(f childFrame) (bool, error) {
		accepted, waiting := pending.reserve(f.ID)
		if !accepted {
			if waiting {
				return false, nil
			}
			return false, errProtocol
		}
		seen[f.ID] = true
		go func() {
			if ctx.Err() != nil {
				return
			}
			result, err := helper(f.Key)
			if ctx.Err() != nil {
				return
			}
			reply := struct {
				ID     string `json:"id"`
				OK     bool   `json:"ok"`
				Result any    `json:"result"`
			}{f.ID, err == nil, result}
			data, marshalErr := json.Marshal(reply)
			if marshalErr != nil || len(data)+1 > 2<<20 {
				p.Abort()
				select {
				case helperErrors <- errRuntimeLimit:
				case <-ctx.Done():
				}
				return
			}
			data = append(data, '\n')
			pending.mu.Lock()
			pending.ids[f.ID] = replyQueued
			pending.mu.Unlock()
			select {
			case jobs <- writeJob{data, f.ID}:
			case <-ctx.Done():
			}
		}()
		return true, nil
	}
	ready, final, eof, stderrEOF, exited := false, false, false, false, false
	var waitingCall, waitingFinal *childFrame
	var values []collect.Value
	processDone := p.Done
	for {
		if waitingCall != nil {
			started, err := startCall(*waitingCall)
			if err != nil {
				return nil, err
			}
			if started {
				waitingCall = nil
			}
		}
		if waitingFinal != nil {
			complete, invalid := pending.finalReady()
			if invalid {
				return nil, errProtocol
			}
			if complete {
				values, final, waitingFinal = waitingFinal.Values, true, nil
				if finish != nil {
					if err := finish(); err != nil {
						return nil, errors.New("runtime network failed")
					}
				}
				if err := p.Stdin.Close(); err != nil {
					return nil, errProtocol
				}
			}
		}
		if final && eof && stderrEOF && exited {
			return values, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-proxyFailure:
			return nil, errors.New("runtime network failed")
		case err := <-helperErrors:
			return nil, err
		case err := <-written:
			if err != nil {
				return nil, errProtocol
			}
		case err := <-stderr:
			if err != nil {
				return nil, errRuntimeLimit
			}
			stderrEOF = true
			stderr = nil
		case <-processDone:
			if p.WaitErr() != nil {
				return nil, errors.New("runtime child failed")
			}
			exited = true
			processDone = nil
		case event := <-frames:
			if event.err != nil {
				if event.err == io.EOF && (final || waitingFinal != nil) {
					eof = true
					frames = nil
					continue
				}
				return nil, errProtocol
			}
			f := event.frame
			if final || waitingFinal != nil || waitingCall != nil {
				return nil, errProtocol
			}
			switch f.Type {
			case "ready":
				if ready {
					return nil, errProtocol
				}
				ready = true
				if err := verify(ctx); err != nil {
					return nil, errors.New("runtime sandbox verification failed")
				}
				input := struct {
					Phase    string          `json:"phase"`
					SourceID string          `json:"source_id"`
					Keys     []collect.Key   `json:"keys"`
					Values   []collect.Value `json:"values"`
				}{inv.Phase, inv.SourceID, inv.Keys, inv.Values}
				if input.Keys == nil {
					input.Keys = []collect.Key{}
				}
				if input.Values == nil {
					input.Values = []collect.Value{}
				}
				data, err := json.Marshal(input)
				if err != nil || len(data)+1 > 65536 {
					return nil, errRuntimeLimit
				}
				data = append(data, '\n')
				pending.mu.Lock()
				pending.ids[""] = replyQueued
				pending.mu.Unlock()
				jobs <- writeJob{data: data}
			case "call":
				if !ready || inv.Phase != "source" || helper == nil || !allowed[f.Key] || seen[f.ID] {
					return nil, errProtocol
				}
				started, err := startCall(f)
				if err != nil {
					return nil, err
				}
				if !started {
					waitingCall = &f
				}
			case "final":
				if !ready {
					return nil, errProtocol
				}
				waitingFinal = &f
			}
		}
	}
}
