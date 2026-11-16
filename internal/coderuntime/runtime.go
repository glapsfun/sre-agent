package coderuntime

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/collect"
)

//go:embed bootstrap.js
var bootstrap []byte

type runtimeAdapter struct{}

func New() collect.Runtime { return runtimeAdapter{} }
func unavailable() error {
	return fmt.Errorf("%w: Bun sandbox unavailable", collect.ErrCodeUnavailable)
}

func (r runtimeAdapter) Probe(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok || ctx.Err() != nil {
		return errors.Join(unavailable(), ctx.Err())
	}
	executionDeadline, reserve, err := probeDeadlines(time.Now(), deadline)
	if err != nil {
		return errors.Join(unavailable(), ctx.Err())
	}
	execution, cancel := context.WithDeadline(ctx, executionDeadline)
	s, runErr := r.Open(execution)
	if runErr == nil {
		_, runErr = s.Run(execution, collect.Invocation{Phase: "join", Program: "return [];"}, nil)
	}
	cancel()
	var closeErr error
	if s != nil {
		cleanupDeadline := time.Now().Add(reserve)
		if cleanupDeadline.After(deadline) {
			cleanupDeadline = deadline
		}
		cleanup, stop := context.WithDeadline(context.WithoutCancel(ctx), cleanupDeadline)
		closeErr = s.Close(cleanup)
		stop()
	}
	if runErr != nil || closeErr != nil || ctx.Err() != nil {
		return errors.Join(unavailable(), ctx.Err())
	}
	return nil
}

// Probe has no external session owner, so it reserves cleanup inside the
// caller's absolute deadline even when that caller cancels early.
func probeDeadlines(now, deadline time.Time) (time.Time, time.Duration, error) {
	remaining := deadline.Sub(now)
	if remaining <= 100*time.Millisecond {
		return time.Time{}, 0, unavailable()
	}
	reserve := min(2*time.Second, remaining/2)
	return deadline.Add(-reserve), reserve, nil
}
func (runtimeAdapter) Open(ctx context.Context) (collect.Session, error) {
	cfg, err := discoverSandbox(ctx)
	if err != nil {
		return nil, errors.Join(unavailable(), ctx.Err())
	}
	dir, err := os.MkdirTemp("", "sre-bun-")
	if err != nil {
		return nil, unavailable()
	}
	s := &runtimeSession{ctx: ctx, cfg: cfg, dir: dir, proxyBudget: newProxyBudget(), gate: make(chan struct{}, 1)}
	if err := os.WriteFile(filepath.Join(dir, "bootstrap.js"), bootstrap, 0600); err != nil {
		return s, unavailable()
	}
	if ctx.Err() != nil {
		return s, errors.Join(unavailable(), ctx.Err())
	}
	return s, nil
}

type runtimeSession struct {
	mu                      sync.Mutex
	ctx                     context.Context
	cfg                     sandboxConfig
	dir                     string
	running, failed, closed bool
	cancel                  context.CancelFunc
	runDone                 chan struct{}
	processes               []*sandboxProcess
	proxyBudget             *proxyBudget
	protocolBudget          protocolBudget
	gate                    chan struct{}
}

func validateInvocation(inv collect.Invocation, helper collect.Helper) error {
	if len(inv.Program) > 65536 || !utf8.ValidString(inv.Program) {
		return errProtocol
	}
	switch inv.Phase {
	case "source":
		if inv.SourceID == "" || len(inv.Keys) == 0 || len(inv.Values) != 0 || helper == nil {
			return errProtocol
		}
		if _, err := normalizeAuthority(inv.HostPort, ""); err != nil {
			return errProtocol
		}
		seen := map[string]bool{}
		for _, key := range inv.Keys {
			if key.Key == "" || key.SourceID != inv.SourceID || key.Tool == "" || seen[key.Key] {
				return errProtocol
			}
			seen[key.Key] = true
		}
	case "join":
		if inv.SourceID != "" || inv.HostPort != "" || len(inv.Keys) != 0 || helper != nil {
			return errProtocol
		}
	default:
		return errProtocol
	}
	return nil
}

func (s *runtimeSession) Run(ctx context.Context, inv collect.Invocation, helper collect.Helper) (execution collect.Execution, err error) {
	s.mu.Lock()
	if s.running || s.closed || s.failed {
		s.mu.Unlock()
		return execution, errors.New("runtime session unavailable")
	}
	s.running = true
	s.runDone = make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	stop := context.AfterFunc(s.ctx, cancel)
	s.mu.Unlock()
	defer func() {
		if err != nil {
			err = errors.Join(err, ctx.Err(), s.ctx.Err())
		}
		stop()
		cancel()
		s.mu.Lock()
		s.running = false
		s.cancel = nil
		if err != nil {
			s.failed = true
		}
		close(s.runDone)
		s.mu.Unlock()
	}()
	if boundedSandboxContext(ctx) != nil || s.ctx.Err() != nil {
		return execution, errors.New("runtime context unavailable")
	}
	if err = validateInvocation(inv, helper); err != nil {
		return execution, err
	}
	dir, e := os.MkdirTemp(s.dir, "phase-")
	if e != nil {
		return execution, errors.New("runtime files unavailable")
	}
	program := filepath.Join(dir, "program.js")
	if e := os.WriteFile(program, []byte(inv.Program), 0600); e != nil {
		return execution, errors.New("runtime files unavailable")
	}
	socket := ""
	var proxy *egressProxy
	var proxyFailure <-chan struct{}
	var finish func() error
	if inv.Phase == "source" {
		socket = filepath.Join(dir, "egress.sock")
		proxy, e = startProxy(ctx, socket, inv, s.proxyBudget)
		if e != nil {
			return execution, errors.New("runtime proxy unavailable")
		}
		proxyFailure = proxy.Failed()
		finish = func() error { _, err := proxy.Close(); return err }
		defer func() {
			audit, closeErr := proxy.Close()
			execution.Audit = audit
			if closeErr != nil && err == nil {
				err = errors.New("runtime network failed")
			}
		}()
	}
	p, e := launchSandbox(ctx, s.cfg, filepath.Join(s.dir, "bootstrap.js"), program, socket)
	if p != nil {
		s.mu.Lock()
		s.processes = append(s.processes, p)
		s.mu.Unlock()
		defer p.Abort()
	}
	if e != nil {
		return execution, errors.New("runtime launch failed")
	}
	execution.Values, err = runProtocol(ctx, p, &s.protocolBudget, inv, helper, p.Verify, proxyFailure, finish)
	return execution, err
}

func (s *runtimeSession) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	// Cancellation makes Run release ownership before the cleanup snapshot.
	s.mu.Lock()
	running, done := s.running, s.runDone
	s.mu.Unlock()
	if running {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
	}
	s.mu.Lock()
	processes := append([]*sandboxProcess{}, s.processes...)
	s.mu.Unlock()
	for _, p := range processes {
		if err := p.Close(ctx); err != nil {
			return errors.Join(errors.New("runtime cleanup failed"), ctx.Err())
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(s.dir); err != nil {
		return errors.New("runtime file cleanup failed")
	}
	return nil
}
