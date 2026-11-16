package coderuntime

import (
	"strings"
	"testing"
)

func TestProtocolFrames(t *testing.T) {
	for _, s := range []string{`{"type":"ready"}`, `{"type":"call","id":"a_1-Z","key":"own"}`, `{"type":"final","values":[{"key":"own","source_id":"s","data":{"a":1}}]}`} {
		if _, err := parseFrame([]byte(s)); err != nil {
			t.Errorf("valid frame: %v", err)
		}
	}
	for _, s := range []string{
		`{"type":"ready","audit":[]}`, `{"type":"ready","type":"ready"}`,
		`{"type":"call","id":"a","key":"own","arguments":{}}`, `{"type":"call","id":"","key":"own"}`,
		`{"type":"call","id":"a.b","key":"own"}`, `{"type":"call","id":"` + strings.Repeat("x", 65) + `","key":"own"}`,
		`{"type":"final","values":null}`, `{"type":"final","values":[{"key":"a","source_id":"s","data":{"x":1,"x":2}}]}`,
		`{"type":"final","values":[{}]}`, `{"type":"final","values":[{"key":"a","source_id":"s"}]}`,
		`{"type":"final","values":[{"key":"a","source_id":"s","data":{},"audit":[]}]}`,
		`{"type":"ready"} {}`, "{\"type\":\"\xff\"}",
		`{"type":"final","values":` + strings.Repeat("[", 65) + strings.Repeat("]", 65) + `}`,
	} {
		if _, err := parseFrame([]byte(s)); err == nil {
			t.Errorf("accepted invalid frame %q", s)
		}
	}
}

func TestProtocolExactBounds(t *testing.T) {
	for _, n := range []int{64, 65} {
		data := []byte(strings.Repeat("[", n) + "0" + strings.Repeat("]", n))
		if (validJSON(data) == nil) != (n == 64) {
			t.Fatalf("depth %d", n)
		}
	}
	for _, n := range []int{4096, 4097} {
		s := `{"type":"ready"}`
		s += strings.Repeat(" ", n-len(s))
		_, err := parseFrame([]byte(s))
		if (err == nil) != (n == 4096) {
			t.Fatalf("ready bytes %d: %v", n, err)
		}
	}
	for _, n := range []int{65536, 65537} {
		prefix := `[{"key":"a","source_id":"s","data":"`
		suffix := `"}]`
		values := prefix + strings.Repeat("a", n-len(prefix)-len(suffix)) + suffix
		_, err := parseFrame([]byte(`{"type":"final","values":` + values + `}`))
		if (err == nil) != (n == 65536) {
			t.Fatalf("values bytes %d: %v", n, err)
		}
	}
	for _, tc := range []struct {
		kind  string
		limit int
	}{{"host", 32 << 20}, {"stderr", 64 << 10}, {"child", 256 << 10}} {
		b := &protocolBudget{}
		if err := b.consume(tc.kind, tc.limit); err != nil {
			t.Fatal(err)
		}
		if b.consume(tc.kind, 1) == nil {
			t.Fatalf("accepted excess %s bytes", tc.kind)
		}
	}
	b := &protocolBudget{}
	for range 128 {
		if b.consume("child", 1) != nil {
			t.Fatal("early frame exhaustion")
		}
	}
	if b.consume("child", 1) == nil {
		t.Fatal("accepted frame 129")
	}
}
