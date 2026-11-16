package collect

import (
	"encoding/json"
	"errors"
	"unicode"
	"unicode/utf8"
)

var errOutput = errors.New("invalid collection output")

func (c *collection) redactValue(v Value) (Value, error) {
	if !validJSON(v.Data) {
		return Value{}, errOutput
	}
	// The array envelope preserves JSON scalar quoting during text redaction.
	raw := c.redactor.Redact("[" + string(v.Data) + "]")
	var data []json.RawMessage
	if !validJSON([]byte(raw)) || json.Unmarshal([]byte(raw), &data) != nil || len(data) != 1 {
		return Value{}, errOutput
	}
	v.Data = append(json.RawMessage{}, data[0]...)
	return v, nil
}
func valuesFit(values []Value) bool {
	raw, err := json.Marshal(values)
	return err == nil && len(raw) <= maxBytes
}
func (c *collection) validateValues(values []Value, allowed map[string]string) ([]Value, error) {
	if len(values) > len(allowed) {
		return nil, errOutput
	}
	out := []Value{}
	seen := map[string]bool{}
	for _, v := range values {
		id, ok := allowed[v.Key]
		if !ok || id != v.SourceID || seen[v.Key] {
			return nil, errOutput
		}
		seen[v.Key] = true
		value, err := c.redactValue(v)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	if !valuesFit(out) {
		return nil, errOutput
	}
	return out, nil
}
func (c *collection) acceptAudit(events []AuditEvent, phase, id string) bool {
	valid := true
	for _, event := range events {
		if event.At.IsZero() || event.At.Year() < 1 || event.At.Year() > 9999 || !auditText(event.Permission, 64) || event.Permission == "" || !auditText(event.Value, 2048) {
			valid = false
			continue
		}
		event.Phase = phase
		event.SourceID = id
		event.Permission = c.redactor.Redact(event.Permission)
		event.Value = c.redactor.Redact(event.Value)
		if !auditText(event.Permission, 64) || !auditText(event.Value, 2048) {
			valid = false
			continue
		}
		raw, err := json.Marshal(event)
		proposedBytes := c.auditBytes + len(raw)
		if len(c.result.Audit) > 0 {
			proposedBytes++
		}
		if err != nil || proposedBytes+2 > 1<<20 {
			valid = false
			break
		}
		c.result.Audit = append(c.result.Audit, event)
		c.auditBytes = proposedBytes
	}
	return valid
}
func auditText(value string, limit int) bool {
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
