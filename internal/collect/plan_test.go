package collect

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodePlanStrictEnvelopeAndArguments(t *testing.T) {
	valid := `{"calls":[{"key":"a","source_id":"s","tool":"read","arguments":{"X":1,"x":9007199254740993}}],"required":["a"],"operation":"filter"}`
	p, err := DecodePlan([]byte(valid))
	if err != nil || !strings.Contains(string(p.Calls[0].Arguments), "9007199254740993") {
		t.Fatalf("valid exact arguments: %+v %v", p, err)
	}
	for _, raw := range []string{
		strings.Replace(valid, `"calls"`, `"Calls"`, 1),
		strings.Replace(valid, `"key"`, `"Key"`, 1),
		strings.Replace(valid, `"key":"a"`, `"key":"a","key":"b"`, 1),
		strings.Replace(valid, `"X":1`, `"x":1`, 1),
		strings.Replace(valid, `"operation":"filter"`, `"operation":"filter","extra":0`, 1),
		strings.Replace(valid, `"required":["a"]`, `"required":[]`, 1),
		strings.Replace(valid, `"required":["a"]`, `"required":["a","a"]`, 1),
		strings.Replace(valid, `"required":["a"]`, `"required":["b"]`, 1),
		strings.Replace(valid, `"operation":"filter"`, `"operation":"unknown"`, 1),
		strings.Replace(valid, `"X":1`, `"X":`+strings.Repeat("[", 64)+"0"+strings.Repeat("]", 64), 1),
		valid + "{}", "null", strings.Replace(valid, "filter", string([]byte{0xff}), 1), strings.Repeat(" ", 65536) + valid,
	} {
		if _, err := DecodePlan([]byte(raw)); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("accepted invalid plan %q: %v", raw, err)
		}
	}
	p, err = DecodePlan([]byte(`{"calls":[{"key":"a","source_id":"s","tool":"read"}],"required":["a"],"operation":"independent"}`))
	if err != nil || string(p.Calls[0].Arguments) != "{}" {
		t.Fatalf("default arguments: %+v %v", p, err)
	}
}

func TestValidatePlanBounds(t *testing.T) {
	p := Plan{Required: []string{"a"}, Operation: "join"}
	for i := 0; i < 32; i++ {
		p.Calls = append(p.Calls, Call{Key: string(rune('a' + i)), SourceID: "s", Tool: "read", Arguments: json.RawMessage(`{}`)})
	}
	// Safe keys rather than punctuation after z.
	for i := 26; i < 32; i++ {
		p.Calls[i].Key = "key" + string(rune('a'+i-26))
	}
	if err := ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
	p.Calls = append(p.Calls, Call{Key: "extra", SourceID: "s", Tool: "read"})
	if !errors.Is(ValidatePlan(p), ErrInvalidPlan) {
		t.Fatal("accepted 33 calls")
	}
	p.Calls = p.Calls[:9]
	for i := range p.Calls {
		p.Calls[i].SourceID = p.Calls[i].Key
	}
	if !errors.Is(ValidatePlan(p), ErrInvalidPlan) {
		t.Fatal("accepted 9 sources")
	}
}
