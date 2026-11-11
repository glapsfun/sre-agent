package executil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/glapsfun/sre-agent/internal/safety"
)

const DefaultTimeout = 30 * time.Second

type Result struct {
	Stdout, Stderr string
	Err            error
}

type Runner struct {
	Timeout time.Duration
	Secrets []string
}

func (r Runner) Run(ctx context.Context, name string, args ...string) Result {
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
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
	result := Result{Stdout: redactor.Redact(stdout.String()), Stderr: redactor.Redact(stderr.String())}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			result.Err = err
		} else {
			// os/exec errors can include paths or arguments containing credentials.
			result.Err = fmt.Errorf("external command failed: %s", redactor.Redact(err.Error()))
		}
	}
	return result
}
