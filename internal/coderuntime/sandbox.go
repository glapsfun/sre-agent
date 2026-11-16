package coderuntime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
)

type sandboxConfig struct {
	bun, bwrap, systemdRun, systemctl, prlimit, env string
	libraries                                       []sandboxLibrary
	managerEnv                                      []string
}
type sandboxLibrary struct{ source, target string }

type sandboxProcess struct {
	Stdin     io.WriteCloser
	Stdout    io.ReadCloser
	Stderr    io.ReadCloser
	Done      <-chan struct{}
	cmd       *exec.Cmd
	process   *os.Process
	aborted   bool
	cfg       sandboxConfig
	unit      string
	startTime string
	mu        sync.Mutex
	cgroup    string
	waitErr   error
	abortOnce sync.Once
	closeGate chan struct{}
	closed    bool
}

func (p *sandboxProcess) WaitErr() error {
	<-p.Done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}
func (p *sandboxProcess) Abort() {
	p.abortOnce.Do(func() {
		p.mu.Lock()
		p.aborted = true
		process := p.process
		p.mu.Unlock()
		if process != nil {
			_ = process.Kill()
		}
		_ = p.Stdin.Close()
		_ = p.Stdout.Close()
		_ = p.Stderr.Close()
	})
}
func boundedSandboxContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("sandbox requires a bounded context")
	}
	return nil
}
