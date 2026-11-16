package coderuntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/glapsfun/sre-agent/internal/collect"
)

const (
	proxyMaxRequests = 64
	proxyMaxActive   = 8
	proxyMaxBytes    = 32 << 20
	proxyMaxAudit    = 1 << 20
	proxyMaxHeader   = 16 << 10
	proxyCopyBuffer  = 32 << 10
)

var errProxyLimit = errors.New("proxy limit exceeded")
var errProxyAudit = errors.New("proxy audit limit exceeded")

type proxyBudget struct {
	mu                                                         sync.Mutex
	requests, active, bytes, reserved, auditBytes, auditEvents int
	changed                                                    chan struct{}
}

func newProxyBudget() *proxyBudget { return &proxyBudget{auditBytes: 2, changed: make(chan struct{})} }
func (b *proxyBudget) request() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.requests >= proxyMaxRequests {
		return false
	}
	b.requests++
	return true
}
func (b *proxyBudget) acquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active >= proxyMaxActive {
		return false
	}
	b.active++
	return true
}
func (b *proxyBudget) release() { b.mu.Lock(); b.active--; b.mu.Unlock() }
func (b *proxyBudget) reserve(ctx context.Context, want int) (int, error) {
	for {
		b.mu.Lock()
		available := proxyMaxBytes - b.bytes - b.reserved
		if available > 0 {
			n := min(want, available)
			b.reserved += n
			b.mu.Unlock()
			return n, nil
		}
		if b.bytes >= proxyMaxBytes {
			b.mu.Unlock()
			return 0, errProxyLimit
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-changed:
		}
	}
}
func (b *proxyBudget) consume(ctx context.Context, n int) error {
	for {
		b.mu.Lock()
		if b.bytes+n > proxyMaxBytes {
			b.mu.Unlock()
			return errProxyLimit
		}
		if b.bytes+b.reserved+n <= proxyMaxBytes {
			b.bytes += n
			b.mu.Unlock()
			return nil
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (b *proxyBudget) settle(reserved, processed int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserved -= reserved
	b.bytes += processed
	close(b.changed)
	b.changed = make(chan struct{})
}
func (b *proxyBudget) audit(event collect.AuditEvent) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return errProxyAudit
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	size := len(encoded)
	if b.auditEvents > 0 {
		size++
	}
	if b.auditBytes+size > proxyMaxAudit {
		return errProxyAudit
	}
	b.auditBytes += size
	b.auditEvents++
	return nil
}
