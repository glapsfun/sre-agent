package executil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/glapsfun/sre-agent/internal/safety"
)

const DefaultTimeout = 30 * time.Second

type Result struct {
	Stdout, Stderr string
	Err            error
	// ExitCode is present only for a normal, nonzero process exit. Launch,
	// cancellation, signal, and postprocessing failures leave it unset.
	ExitCode *int
}

type Runner struct {
	Timeout time.Duration
	Secrets []string
}

func (r Runner) Run(ctx context.Context, name string, args ...string) Result {
	return r.execute(ctx, nil, name, args...)
}

// Stream sends raw stdout to a trusted consumer, which must redact it before
// retention or presentation. Stderr and errors retain Run's redaction rules.
// The consumer must return promptly from Write, including during cancellation.
func (r Runner) Stream(ctx context.Context, output io.Writer, name string, args ...string) Result {
	if output == nil {
		return Result{Err: errors.New("stream output is required")}
	}
	return r.execute(ctx, output, name, args...)
}

func (r Runner) execute(ctx context.Context, output io.Writer, name string, args ...string) Result {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// A descendant may keep inherited pipes open after its parent exits.
	cmd.WaitDelay = 100 * time.Millisecond
	configureProcess(cmd)
	stdout, stderr := responseBuffer{cancel: cancel}, responseBuffer{cancel: cancel}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if output != nil {
		cmd.Stdout = cancelWriter{output: output, cancel: cancel}
	}
	err := cmd.Run()
	// WaitDelay closes inherited pipes after an early parent exit, but its
	// context watcher has already stopped and cannot clean up descendants.
	if cmd.Process != nil {
		_ = cmd.Cancel()
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	redactor := safety.NewRedactor(r.Secrets...)
	if stdout.exceeded || stderr.exceeded {
		return Result{Err: safety.ErrResponseTooLarge}
	}
	if ctx.Err() != nil {
		return Result{Err: ctx.Err()}
	}
	cleanOut, redactErr := redactor.RedactContext(ctx, stdout.String())
	if redactErr != nil {
		return Result{Err: redactErr}
	}
	cleanErr, redactErr := redactor.RedactContext(ctx, stderr.String())
	if redactErr != nil {
		return Result{Err: redactErr}
	}
	result := Result{Stdout: cleanOut, Stderr: cleanErr}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() >= 0 {
			code := exit.ExitCode()
			result.ExitCode = &code
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			result.Err = err
		} else {
			// os/exec errors can include paths or arguments containing credentials.
			message, redactErr := redactor.RedactContext(ctx, err.Error())
			if redactErr != nil {
				return Result{Err: redactErr}
			}
			result.Err = fmt.Errorf("external command failed: %s", message)
		}
	}
	return result
}

type cancelWriter struct {
	output io.Writer
	cancel context.CancelFunc
}

func (w cancelWriter) Write(p []byte) (int, error) {
	n, err := w.output.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.cancel()
	}
	return n, err
}

// Reject oversized output instead of letting a subprocess exhaust memory before
// the redactor can enforce its response limit.
type responseBuffer struct {
	buffer   bytes.Buffer
	exceeded bool
	cancel   context.CancelFunc
}

func (b *responseBuffer) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > safety.MaxResponseBytes {
		b.exceeded = true
		b.cancel()
		return 0, safety.ErrResponseTooLarge
	}
	return b.buffer.Write(p)
}

func (b *responseBuffer) String() string { return b.buffer.String() }
