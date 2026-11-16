package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The wire fixture exposes pagination and malformed replies that an SDK server
// intentionally prevents applications from producing.
func wireServer(t *testing.T, list func(http.ResponseWriter, json.RawMessage, map[string]any), calls *atomic.Int32) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(204)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"wire","version":"1"}}}`, req.ID)
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			list(w, req.ID, req.Params)
		default:
			t.Errorf("unexpected MCP method %s", req.Method)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}
func TestMCPRejectsInvalidInventories(t *testing.T) {
	cases := []struct {
		name, result string
		wantCalls    int32
	}{
		{"repeated cursor", `{"tools":[],"nextCursor":"same"}`, 2},
		{"duplicate tools", `{"tools":[{"name":"x","inputSchema":{}},{"name":"x","inputSchema":{}}]}`, 1},
		{"unsafe name", `{"tools":[{"name":"bad\nname","inputSchema":{}}]}`, 1},
		{"nil descriptor", `{"tools":[null]}`, 1},
		{"invalid header annotation", `{"tools":[{"name":"x","inputSchema":{"type":"object","properties":{"secret":{"type":"object","x-mcp-header":"bad"}}}}]}`, 1},
		{"missing schema", `{"tools":[{"name":"x"}]}`, 1},
		{"scalar schema", `{"tools":[{"name":"x","inputSchema":false}]}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			ts := wireServer(t, func(w http.ResponseWriter, id json.RawMessage, _ map[string]any) {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, id, tc.result)
			}, &calls)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _, cleanup, err := (Service{}).openSession(ctx, httpSource("x", ts.URL))
			if cleanup != nil {
				cleanup()
			}
			if err == nil || calls.Load() != tc.wantCalls {
				t.Fatalf("err=%v calls=%d", err, calls.Load())
			}
		})
	}
}
func TestMCPPaginationAndToolLimits(t *testing.T) {
	for _, many := range []bool{false, true} {
		t.Run(fmt.Sprint(many), func(t *testing.T) {
			var calls atomic.Int32
			ts := wireServer(t, func(w http.ResponseWriter, id json.RawMessage, _ map[string]any) {
				tools := []map[string]any{}
				if many {
					for i := range 101 {
						tools = append(tools, map[string]any{"name": fmt.Sprintf("t%d", i), "inputSchema": map[string]any{}})
					}
				}
				result := map[string]any{"tools": tools, "nextCursor": fmt.Sprint(calls.Load())}
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
			}, &calls)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _, cleanup, err := (Service{}).openSession(ctx, httpSource("x", ts.URL))
			if cleanup != nil {
				cleanup()
			}
			want := int32(10)
			if many {
				want = 1
			}
			if err == nil || calls.Load() != want {
				t.Fatalf("err=%v calls=%d", err, calls.Load())
			}
		})
	}
}
func TestMCPResponseBoundsAndNoRetry(t *testing.T) {
	for _, mode := range []string{"length", "chunked", "sse", "transient"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			ts := wireServer(t, func(w http.ResponseWriter, id json.RawMessage, _ map[string]any) {
				switch mode {
				case "length":
					w.Header().Set("Content-Length", fmt.Sprint((1<<20)+1))
					w.WriteHeader(200)
				case "chunked":
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					io.WriteString(w, strings.Repeat(" ", (1<<20)+1))
				case "sse":
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					io.WriteString(w, "data: "+strings.Repeat("x", (1<<20)+1)+"\n\n")
				case "transient":
					w.WriteHeader(503)
				}
			}, &calls)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _, cleanup, err := (Service{}).openSession(ctx, httpSource("x", ts.URL))
			if cleanup != nil {
				cleanup()
			}
			if err == nil || calls.Load() != 1 {
				t.Fatalf("err=%v calls=%d", err, calls.Load())
			}
		})
	}
}
func TestMCPRedirectAndDeadline(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { t.Error("caller redirect policy used"); return nil }}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _, err := (Service{HTTPClient: client}).openSession(ctx, httpSource("x", redirect.URL))
	if err == nil || reached.Load() != 0 || client.Transport != nil || client.CheckRedirect == nil {
		t.Fatalf("err=%v reached=%d", err, reached.Load())
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer slow.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	_, _, _, err = (Service{}).openSession(ctx2, httpSource("x", slow.URL))
	if err != context.DeadlineExceeded {
		t.Fatal(err)
	}
}

func TestMCPCancellationBeforeCredentialLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := httpSource("x", "http://127.0.0.1:1")
	src.CredentialEnv = "TOKEN"
	_, _, _, err := (Service{LookupEnv: func(string) (string, bool) { t.Error("canceled request resolved credentials"); return "secret", true }}).openSession(ctx, src)
	if err != context.Canceled {
		t.Fatal(err)
	}
}
func TestMCPBoundsInjectedTransportAndOutgoingBody(t *testing.T) {
	var requests atomic.Int32
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", (1<<20)+1))), ContentLength: -1}, nil
	})
	transport := &sourceTransport{base: rt, ctx: context.Background(), endpoint: "https://approved.example/mcp"}
	req, _ := http.NewRequest("POST", "https://approved.example/mcp", strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	if _, err := transport.RoundTrip(req); err == nil || requests.Load() != 0 {
		t.Fatal("oversized request reached transport")
	}
	req, _ = http.NewRequest("POST", "https://other.example/mcp", nil)
	if _, err := transport.RoundTrip(req); err == nil || requests.Load() != 0 {
		t.Fatal("unpinned endpoint reached transport")
	}
	req, _ = http.NewRequest("POST", "https://approved.example/mcp", nil)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err == nil || len(raw) > 1<<20 {
		t.Fatal("injected response not bounded")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestMCPSSEAggregateBound(t *testing.T) {
	var calls atomic.Int32
	ts := wireServer(t, func(w http.ResponseWriter, _ json.RawMessage, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		for range 20000 {
			if _, err := io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n"); err != nil {
				return
			}
		}
	}, &calls)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, cleanup, err := (Service{}).openSession(ctx, httpSource("x", ts.URL))
	if cleanup != nil {
		cleanup()
	}
	if err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}
