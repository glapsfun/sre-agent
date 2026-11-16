package collect

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/glapsfun/sre-agent/internal/sources"
)

var errHelper = errors.New("collection helper unavailable")

type cachedCall struct {
	done   chan struct{}
	result sources.CallResult
	err    error
}
type helperScope struct {
	source  string
	ctx     context.Context
	cancel  context.CancelFunc
	sealed  bool
	pending int
	done    chan struct{}
}
type helpers struct {
	mu                        sync.Mutex
	collection                *collection
	ctx                       context.Context
	cancel                    context.CancelFunc
	semaphore                 chan struct{}
	entries                   map[string]*cachedCall
	calls                     map[string]Call
	receipts                  map[string]Activity
	gaps                      []Gap
	cacheBytes, metadataBytes int
	fatal, frozen             bool
}

func newHelpers(ctx context.Context, c *collection) *helpers {
	ctx, cancel := context.WithCancel(ctx)
	h := &helpers{collection: c, ctx: ctx, cancel: cancel, semaphore: make(chan struct{}, 2), entries: map[string]*cachedCall{}, calls: map[string]Call{}, receipts: map[string]Activity{}}
	for _, call := range c.plan.Calls {
		h.calls[call.Key] = call
	}
	return h
}
func (h *helpers) scope(source string) *helperScope {
	ctx, cancel := context.WithCancel(h.ctx)
	return &helperScope{source: source, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}
func (h *helpers) failLocked(reason string) {
	if h.frozen {
		return
	}
	if !h.fatal {
		h.gaps = append(h.gaps, Gap{Reason: reason, Impact: "Code collection stopped; output may be incomplete."})
	}
	h.fatal = true
	h.cancel()
}
func cloneCall(r sources.CallResult) sources.CallResult {
	r.Data = append(json.RawMessage(nil), r.Data...)
	r.Gaps = append([]sources.Gap{}, r.Gaps...)
	return r
}
func (h *helpers) call(scope *helperScope, key string) (sources.CallResult, error) {
	h.mu.Lock()
	call, ok := h.calls[key]
	if !ok || call.SourceID != scope.source || scope.sealed || h.frozen {
		h.failLocked("invalid or sealed helper key")
		h.mu.Unlock()
		return sources.CallResult{}, errHelper
	}
	if entry, exists := h.entries[key]; exists {
		h.mu.Unlock()
		// Cancellation is a cached source outcome too; wait for its single owner.
		<-entry.done
		return cloneCall(entry.result), entry.err
	}
	entry := &cachedCall{done: make(chan struct{})}
	h.entries[key] = entry
	scope.pending++
	h.mu.Unlock()
	var r sources.CallResult
	var err error
	var activity *Activity
	select {
	case h.semaphore <- struct{}{}:
		h.mu.Lock()
		allowed := !scope.sealed && !h.frozen && scope.ctx.Err() == nil
		if allowed {
			a := Activity{Key: call.Key, SourceID: call.SourceID, Tool: call.Tool, At: time.Now().UTC(), Gaps: []sources.Gap{{SourceID: call.SourceID, Reason: "source collection interrupted", Impact: "Source evidence is unavailable."}}}
			h.receipts[key] = a
		}
		h.mu.Unlock()
		if allowed {
			start := time.Now()
			r, err = h.collection.invoke(scope.ctx, call)
			a := activityFor(call, r, err, time.Since(start))
			activity = &a
		} else {
			err = errHelper
		}
		<-h.semaphore
	case <-scope.ctx.Done():
		err = scope.ctx.Err()
	}
	h.mu.Lock()
	if !h.frozen {
		if activity != nil {
			h.receipts[key] = *activity
		}
		raw, marshalErr := json.Marshal(r)
		if marshalErr != nil || h.cacheBytes+len(raw) > 16<<20 {
			r.Data = nil
			r.Gaps = []sources.Gap{{SourceID: call.SourceID, Reason: "helper cache budget exceeded", Impact: "Source data was omitted."}}
			err = errHelper
			h.failLocked("helper cache budget exceeded")
		} else {
			h.cacheBytes += len(raw)
		}
		metadata := r
		metadata.Data = nil
		raw, marshalErr = json.Marshal(metadata)
		if marshalErr != nil || h.metadataBytes+len(raw) > 256<<10 {
			r = sources.CallResult{SourceID: call.SourceID, Tool: call.Tool, Gaps: []sources.Gap{{SourceID: call.SourceID, Reason: "helper metadata budget exceeded", Impact: "Source data was omitted."}}}
			err = errHelper
			h.failLocked("helper metadata budget exceeded")
		} else {
			h.metadataBytes += len(raw)
		}
	}
	entry.result = cloneCall(r)
	entry.err = err
	close(entry.done)
	scope.pending--
	if scope.sealed && scope.pending == 0 {
		close(scope.done)
	}
	h.mu.Unlock()
	return cloneCall(entry.result), entry.err
}
func (h *helpers) seal(scope *helperScope) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	scope.sealed = true
	scope.cancel()
	pending := scope.pending > 0
	if pending {
		h.failLocked("runtime returned with pending helpers")
	} else {
		close(scope.done)
	}
	return pending
}
func (h *helpers) failed() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.fatal }
func (h *helpers) freeze() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.frozen = true
	h.cancel()
	for key, r := range h.receipts {
		r.Gaps = append([]sources.Gap{}, r.Gaps...)
		h.collection.receipts[key] = r
	}
	h.collection.result.Gaps = append(h.collection.result.Gaps, h.gaps...)
}
