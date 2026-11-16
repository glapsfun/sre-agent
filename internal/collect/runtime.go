package collect

import (
	"context"
	"encoding/json"
	"time"

	"github.com/glapsfun/sre-agent/internal/sources"
)

func (c *collection) code(ctx context.Context, runtime Runtime, program string) {
	session, err := runtime.Open(ctx)
	c.noteContext(err)
	if session == nil {
		c.gap("", "", "code runtime open failed")
		return
	}
	h := newHelpers(ctx, c)
	var cleanup context.Context
	var cancelCleanup context.CancelFunc
	cleanupContext := func() context.Context {
		if cleanup == nil {
			cleanup, cancelCleanup = context.WithTimeout(context.Background(), 2*time.Second)
		}
		return cleanup
	}
	defer func() {
		h.freeze()
		clean := cleanupContext()
		if e := session.Close(clean); e != nil || clean.Err() != nil {
			c.noteContext(e)
			c.noteContext(clean.Err())
			c.gap("", "", "code runtime cleanup failed")
		}
		cancelCleanup()
	}()
	if err != nil {
		c.gap("", "", "code runtime open failed")
		return
	}
	intermediateBytes := 0
	for _, id := range c.order {
		if ctx.Err() != nil || h.failed() {
			return
		}
		keys := []Key{}
		allowed := map[string]string{}
		for _, call := range c.plan.Calls {
			if call.SourceID == id {
				keys = append(keys, Key{Key: call.Key, SourceID: id, Tool: call.Tool})
				allowed[call.Key] = id
			}
		}
		scope := h.scope(id)
		invocation := Invocation{Phase: "source", SourceID: id, HostPort: c.bound[id].approved.CodeNetwork.HostPort, Program: program, Keys: keys, Values: []Value{}}
		execution, runErr := session.Run(scope.ctx, invocation, func(key string) (r sources.CallResult, e error) { return h.call(scope, key) })
		c.noteContext(runErr)
		pending := h.seal(scope)
		if pending {
			select {
			case <-scope.done:
			case <-cleanupContext().Done():
				c.gap("", id, "helper cleanup incomplete")
			}
		}
		auditOK := c.acceptAudit(execution.Audit, "source", id)
		if !auditOK {
			c.gap("", id, "invalid code permission audit")
		}
		if runErr != nil {
			c.gap("", id, "code execution failed")
		}
		if pending || h.failed() || runErr != nil || !auditOK || ctx.Err() != nil {
			return
		}
		values, e := c.validateValues(execution.Values, allowed)
		encoded, marshalErr := json.Marshal(values)
		if e != nil || marshalErr != nil || intermediateBytes+len(encoded) > maxBytes {
			c.gap("", id, "invalid or oversized code output")
			return
		}
		intermediateBytes += len(encoded)
		c.values = append(c.values, values...)
	}
	if len(c.order) < 2 {
		return
	}
	if ctx.Err() != nil || h.failed() {
		return
	}
	input := struct {
		Phase  string  `json:"phase"`
		Values []Value `json:"values"`
	}{"join", c.values}
	raw, err := json.Marshal(input)
	if err != nil || len(raw) > maxBytes {
		c.gap("", "", "join input budget exceeded")
		return
	}
	allowed := map[string]string{}
	for _, v := range c.values {
		allowed[v.Key] = v.SourceID
	}
	// Runtime receives its own buffers, including join input data.
	values := make([]Value, len(c.values))
	for j, v := range c.values {
		values[j] = v
		values[j].Data = append(json.RawMessage{}, v.Data...)
	}
	execution, runErr := session.Run(h.ctx, Invocation{Phase: "join", Program: program, Keys: []Key{}, Values: values}, nil)
	c.noteContext(runErr)
	auditOK := c.acceptAudit(execution.Audit, "join", "")
	if !auditOK {
		c.gap("", "", "invalid code permission audit")
	}
	if runErr != nil {
		c.gap("", "", "code join failed")
	}
	if runErr != nil || !auditOK || h.failed() || ctx.Err() != nil {
		return
	}
	final, e := c.validateValues(execution.Values, allowed)
	if e != nil {
		c.gap("", "", "invalid or oversized join output")
		return
	}
	c.values = final
}
