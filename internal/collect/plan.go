package collect

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/sources"
)

const maxBytes = 64 << 10

var identifier = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func DecodePlan(raw []byte) (Plan, error) {
	if len(raw) > maxBytes || !validJSON(raw) {
		return Plan{}, ErrInvalidPlan
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || len(envelope) != 3 {
		return Plan{}, ErrInvalidPlan
	}
	for k := range envelope {
		if k != "calls" && k != "required" && k != "operation" {
			return Plan{}, ErrInvalidPlan
		}
	}
	var calls []map[string]json.RawMessage
	if json.Unmarshal(envelope["calls"], &calls) != nil {
		return Plan{}, ErrInvalidPlan
	}
	for _, call := range calls {
		for k := range call {
			if k != "key" && k != "source_id" && k != "tool" && k != "arguments" {
				return Plan{}, ErrInvalidPlan
			}
		}
	}
	var plan Plan
	if json.Unmarshal(raw, &plan) != nil {
		return Plan{}, ErrInvalidPlan
	}
	for i := range plan.Calls {
		if len(plan.Calls[i].Arguments) == 0 {
			plan.Calls[i].Arguments = json.RawMessage(`{}`)
		}
	}
	if err := ValidatePlan(plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func ValidatePlan(plan Plan) error {
	if len(plan.Calls) == 0 || len(plan.Calls) > 32 || len(plan.Required) == 0 || len(plan.Required) > 32 || (plan.Operation != "independent" && plan.Operation != "filter" && plan.Operation != "join") {
		return ErrInvalidPlan
	}
	keys := map[string]bool{}
	ids := map[string]bool{}
	for _, call := range plan.Calls {
		if !identifier.MatchString(call.Key) || keys[call.Key] || sources.ValidateCallOptions(sources.CallOptions{SourceID: call.SourceID, Tool: call.Tool, Arguments: call.Arguments}) != nil {
			return ErrInvalidPlan
		}
		keys[call.Key] = true
		ids[call.SourceID] = true
	}
	if len(ids) > 8 {
		return ErrInvalidPlan
	}
	required := map[string]bool{}
	for _, key := range plan.Required {
		if !keys[key] || required[key] {
			return ErrInvalidPlan
		}
		required[key] = true
	}
	raw, err := json.Marshal(plan)
	if err != nil || len(raw) > maxBytes || !validJSON(raw) {
		return ErrInvalidPlan
	}
	return nil
}

func validJSON(raw []byte) bool {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if !uniqueJSON(d, 0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func uniqueJSON(d *json.Decoder, depth int) bool {
	token, err := d.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}
	if depth >= 64 {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			token, err := d.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] {
				return false
			}
			seen[key] = true
		}
		if !uniqueJSON(d, depth+1) {
			return false
		}
	}
	_, err = d.Token()
	return err == nil
}
