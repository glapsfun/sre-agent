package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/sources"
)

type boundSource struct {
	approved sources.ApprovedSource
	digest   string
}
type collection struct {
	service    sources.Service
	root       string
	plan       Plan
	bound      map[string]boundSource
	order      []string
	redactor   safety.Redactor
	result     Result
	auditBytes int // Encoded accepted events and commas, excluding array brackets.
	values     []Value
	receipts   map[string]Activity
	contextErr error
}

func (s Service) Collect(parent context.Context, root string, req Request) (result Result, err error) {
	started := time.Now()
	mode := req.Mode
	if mode == "" {
		mode = ModeAuto
	}
	c := &collection{service: s.Sources, root: root, redactor: safety.NewRedactor(), bound: map[string]boundSource{}, receipts: map[string]Activity{}, values: []Value{}, result: Result{RequestedMode: mode, SelectedMode: ModeNone, Evidence: []Evidence{}, Activity: []Activity{}, Audit: []AuditEvent{}, Gaps: []Gap{}, MissingRequired: []string{}, UsageReason: "No model request was made."}}
	defer func() {
		c.finish()
		err = errors.Join(err, c.contextErr)
		c.result.DurationMS = time.Since(started).Milliseconds()
		result = c.result
		if len(result.Gaps) > 0 || len(result.MissingRequired) > 0 {
			err = errors.Join(err, ErrIncompleteCollection)
		}
	}()
	if ValidatePlan(req.Plan) != nil || (mode != ModeAuto && mode != ModeOrdinary && mode != ModeCode) || req.Timeout < 0 || req.Timeout > 60*time.Second || len(req.Program) > maxBytes || !utf8.ValidString(req.Program) {
		c.gap("", "", "invalid collection request")
		return result, ErrInvalidPlan
	}
	// Callers retain their buffers; no runtime callback may change validated bindings.
	c.plan = clonePlan(req.Plan)
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if ctx.Err() != nil {
		c.gap("", "", "collection canceled")
		return result, ctx.Err()
	}
	if e := c.preflight(ctx); e != nil {
		return result, e
	}
	reason := "manual ordinary collection"
	eligible := false
	switch {
	case mode == ModeOrdinary:
	case mode == ModeAuto && (len(c.plan.Calls) < 2 || c.plan.Operation == "independent"):
		reason = "short or independent collection"
	default:
		reason = c.prerequisites(req)
		if ctx.Err() != nil {
			c.gap("", "", "collection canceled")
			return result, ctx.Err()
		}
		if reason == "" {
			if s.Runtime == nil {
				reason = "code runtime unavailable"
			} else if e := s.Runtime.Probe(ctx); ctx.Err() != nil || contextError(e) != nil {
				c.gap("", "", "collection canceled")
				return result, errors.Join(ctx.Err(), contextError(e))
			} else if e != nil {
				reason = "code runtime unavailable"
			} else {
				eligible = true
				reason = "eligible code collection"
			}
		}
		if !eligible && mode == ModeCode {
			c.result.Reason = reason
			c.gap("", "", reason)
			return result, ErrCodeUnavailable
		}
	}
	if ctx.Err() != nil {
		c.gap("", "", "collection canceled")
		return result, ctx.Err()
	}
	c.result.Reason = reason
	if !eligible {
		c.result.SelectedMode = ModeOrdinary
		c.ordinary(ctx)
	} else {
		c.result.SelectedMode = ModeCode
		hash := sha256.Sum256([]byte(req.Program))
		c.result.CodeSHA256 = hex.EncodeToString(hash[:])
		c.code(ctx, s.Runtime, req.Program)
	}
	if ctx.Err() != nil {
		c.gap("", "", "collection canceled")
		return result, ctx.Err()
	}
	return result, nil
}

func clonePlan(p Plan) Plan {
	p.Required = append([]string{}, p.Required...)
	p.Calls = append([]Call{}, p.Calls...)
	for i := range p.Calls {
		if len(p.Calls[i].Arguments) == 0 {
			p.Calls[i].Arguments = json.RawMessage(`{}`)
		} else {
			p.Calls[i].Arguments = append(json.RawMessage{}, p.Calls[i].Arguments...)
		}
	}
	return p
}
func (c *collection) gap(key, id, reason string) {
	c.result.Gaps = append(c.result.Gaps, Gap{Key: key, SourceID: id, Reason: reason, Impact: "Collection evidence is incomplete."})
}
func (c *collection) preflight(ctx context.Context) error {
	registry, err := sources.Load(ctx, c.root)
	if ctx.Err() != nil {
		c.gap("", "", "collection canceled")
		return ctx.Err()
	}
	if err != nil {
		c.gap("", "", "approved source registry unavailable")
		return ErrInvalidPlan
	}
	available := map[string]sources.ApprovedSource{}
	for _, s := range registry.Sources {
		available[s.ID] = s
	}
	for _, call := range c.plan.Calls {
		s, ok := available[call.SourceID]
		if !ok || (s.Kind != "mcp-http" && s.Kind != "utcp-http") || len(s.Endpoint) > 2048 || len(s.Reference) > 2048 {
			c.gap(call.Key, call.SourceID, "source unavailable for collection")
			return ErrInvalidPlan
		}
		found := false
		for _, tool := range s.Tools {
			if tool.Name == call.Tool && tool.ReadOnly {
				found = true
				break
			}
		}
		if !found {
			c.gap(call.Key, call.SourceID, "tool is not approved for read-only collection")
			return ErrInvalidPlan
		}
		if _, ok := c.bound[s.ID]; !ok {
			digest, e := sources.ApprovalDigest(s)
			if e != nil {
				c.gap(call.Key, s.ID, "invalid source approval")
				return ErrInvalidPlan
			}
			c.bound[s.ID] = boundSource{s, digest}
			c.order = append(c.order, s.ID)
		}
	}
	return nil
}
func (c *collection) prerequisites(req Request) string {
	if strings.TrimSpace(req.Program) == "" {
		return "code program missing"
	}
	if !req.CodeApproved {
		return "code consent missing"
	}
	for _, id := range c.order {
		if c.bound[id].approved.CodeNetwork == nil {
			return "code source grant missing"
		}
	}
	lookup := c.service.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	captured := map[string]string{}
	secrets := []string{}
	for _, id := range c.order {
		name := c.bound[id].approved.CredentialEnv
		if _, ok := captured[name]; ok {
			continue
		}
		value, ok := lookup(name)
		if !ok || value == "" || len(value) > maxBytes || strings.ContainsAny(value, "\r\n") || !utf8.ValidString(value) {
			return "code credential unavailable"
		}
		captured[name] = value
		secrets = append(secrets, value)
	}
	c.service.LookupEnv = func(name string) (string, bool) { value, ok := captured[name]; return value, ok }
	c.redactor = safety.NewRedactor(secrets...)
	return ""
}
func (c *collection) invoke(ctx context.Context, call Call) (sources.CallResult, error) {
	if ctx.Err() != nil {
		return sources.CallResult{}, ctx.Err()
	}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return sources.CallResult{}, context.DeadlineExceeded
	}
	if remaining > 60*time.Second {
		remaining = 60 * time.Second
	}
	return c.service.Call(ctx, c.root, sources.CallOptions{SourceID: call.SourceID, Tool: call.Tool, Arguments: append(json.RawMessage{}, call.Arguments...), ExpectedApprovalDigest: c.bound[call.SourceID].digest, Timeout: remaining})
}
func activityFor(call Call, r sources.CallResult, err error, elapsed time.Duration) Activity {
	gaps := append([]sources.Gap{}, r.Gaps...)
	if err != nil && len(gaps) == 0 {
		gaps = append(gaps, sources.Gap{SourceID: call.SourceID, Reason: "source collection failed", Impact: "Source evidence is unavailable."})
	}
	return Activity{Key: call.Key, SourceID: call.SourceID, Tool: call.Tool, Reference: r.Reference, At: r.At, ElapsedMS: elapsed.Milliseconds(), Success: err == nil && len(gaps) == 0, Gaps: gaps}
}
func (c *collection) ordinary(ctx context.Context) {
	for _, call := range c.plan.Calls {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		r, err := c.invoke(ctx, call)
		c.noteContext(err)
		c.receipts[call.Key] = activityFor(call, r, err, time.Since(start))
		if len(r.Data) == 0 {
			continue
		}
		value, e := c.redactValue(Value{Key: call.Key, SourceID: call.SourceID, Data: r.Data})
		if e != nil || !valuesFit(append(append([]Value{}, c.values...), value)) {
			c.gap(call.Key, call.SourceID, "source data omitted: output budget or format")
			continue
		}
		c.values = append(c.values, value)
	}
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return nil
}

func (c *collection) noteContext(err error) {
	if e := contextError(err); e != nil {
		c.contextErr = errors.Join(c.contextErr, e)
	}
}
func (c *collection) finish() {
	retained := map[string]bool{}
	values := map[string]Value{}
	for _, v := range c.values {
		values[v.Key] = v
	}
	for _, call := range c.plan.Calls {
		receipt, called := c.receipts[call.Key]
		if called {
			c.result.Activity = append(c.result.Activity, receipt)
			for _, g := range receipt.Gaps {
				c.result.Gaps = append(c.result.Gaps, Gap{Key: call.Key, SourceID: call.SourceID, Reason: g.Reason, Impact: g.Impact})
			}
		}
		value, ok := values[call.Key]
		if !ok {
			continue
		}
		e := Evidence{Key: call.Key, SourceID: call.SourceID, Data: append(json.RawMessage{}, value.Data...), Origin: "program_only"}
		if called {
			e.Tool = receipt.Tool
			e.Reference = receipt.Reference
			e.At = receipt.At
		}
		if c.result.SelectedMode == ModeCode && (!called || !receipt.Success) {
			e.Tool = ""
			approved := c.bound[call.SourceID].approved
			e.Reference = approved.Reference
			if e.Reference == "" {
				e.Reference = approved.Endpoint
			}
		}
		if c.result.SelectedMode == ModeOrdinary {
			e.Origin = "source"
		} else if called && receipt.Success {
			e.Origin = "program_derived"
		}
		c.result.Evidence = append(c.result.Evidence, e)
		retained[call.Key] = called && receipt.Success
	}
	for _, key := range c.plan.Required {
		if !retained[key] {
			c.result.MissingRequired = append(c.result.MissingRequired, key)
			c.gap(key, c.callSource(key), "required evidence missing")
		}
	}
	c.result.RequiredKeysSatisfied = len(c.plan.Required) > 0 && len(c.result.MissingRequired) == 0
}
func (c *collection) callSource(key string) string {
	for _, call := range c.plan.Calls {
		if call.Key == key {
			return call.SourceID
		}
	}
	return ""
}
