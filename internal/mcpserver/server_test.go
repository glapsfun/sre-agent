package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/discover"
	"github.com/glapsfun/sre-agent/internal/logs"
	"github.com/glapsfun/sre-agent/internal/prom"
	"github.com/glapsfun/sre-agent/internal/store"
	"github.com/glapsfun/sre-agent/internal/timeline"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectTest(t *testing.T, server *Server) *mcp.ClientSession {
	t.Helper()
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTest(t *testing.T, cs *mcp.ClientSession, name, raw string) *mcp.CallToolResult {
	t.Helper()
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestToolSchemas(t *testing.T) {
	var invoked atomic.Int32
	unexpected := func() error { invoked.Add(1); return errors.New("unexpected dispatch") }
	server := newServer(dependencies{
		discover: func(context.Context, discover.Options) (discover.Report, error) {
			return discover.Report{}, unexpected()
		},
		foldPod: func(context.Context, logs.PodOptions) (logs.Report, error) { return logs.Report{}, unexpected() },
		query:   func(context.Context, prom.Options) (prom.Report, error) { return prom.Report{}, unexpected() },
		collect: func(context.Context, timeline.Options) (timeline.Report, error) {
			return timeline.Report{}, unexpected()
		},
		open: func(context.Context, string) (*store.Store, error) { return nil, unexpected() },
	})
	cs := connectTest(t, server)
	listed, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"discover": true, "logs_fold": true, "prom_query": true, "timeline": true, "recall": true, "run_resume": true, "ledger_add": false, "report": true}
	if len(listed.Tools) != len(want) {
		t.Fatalf("tools=%v", listed.Tools)
	}
	for _, tool := range listed.Tools {
		readOnly, ok := want[tool.Name]
		if !ok || tool.Annotations == nil || tool.Annotations.ReadOnlyHint != readOnly {
			t.Fatalf("unexpected tool or annotation: %+v", tool)
		}
		delete(want, tool.Name)
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			AdditionalProperties *bool                      `json:"additionalProperties"`
			Required             []string                   `json:"required"`
			Properties           map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Fatalf("open schema %s: %s", tool.Name, raw)
		}
		if tool.Name == "report" && schema.Properties["out"] != nil {
			t.Fatal("report exposes file writes")
		}
		if tool.Name == "run_resume" && schema.Properties["session_id"] != nil {
			t.Fatal("resume exposes session binding")
		}
		if tool.Name == "discover" || tool.Name == "logs_fold" || tool.Name == "prom_query" || tool.Name == "timeline" {
			if !strings.Contains(string(schema.Properties["context"]), `"minLength":1`) || !contains(schema.Required, "context") {
				t.Fatalf("context not required/nonempty: %s", raw)
			}
		}
	}
	for _, tc := range []struct{ name, input string }{
		{"discover", `{}`}, {"discover", `{"context":""}`}, {"discover", `{"context":"  "}`},
		{"discover", `{"context":"pin","timeout":"0s"}`}, {"discover", `{"context":"pin","timeout":""}`},
		{"discover", `{"context":"pin","only":[]}`}, {"discover", `{"context":"pin","only":["unknown"]}`},
		{"discover", `{"context":"pin","skip_cluster":true}`}, {"discover", `{"context":"pin","context":"other"}`},
		{"logs_fold", `{"text":"logs"}`}, {"logs_fold", `{"context":"","text":"logs"}`},
		{"logs_fold", `{"context":"pin"}`}, {"logs_fold", `{"context":"pin","text":"logs","pod":"api","namespace":"ns"}`},
		{"logs_fold", `{"context":"pin","text":"logs","previous":false}`}, {"logs_fold", `{"context":"pin","text":"logs","max_patterns":0}`},
		{"logs_fold", `{"context":"pin","pod":"api"}`}, {"logs_fold", `{"context":"pin","pod":"api","namespace":"ns","since":"0s"}`},
		{"prom_query", `{"query":"up","url":"http://localhost"}`}, {"prom_query", `{"context":"","query":"up","url":"http://localhost"}`},
		{"prom_query", `{"context":"pin","query":"up","top":0}`}, {"prom_query", `{"context":"pin","query":"up","range":"1ns"}`},
		{"prom_query", `{"context":"pin","query":"up","url":""}`}, {"prom_query", `{"context":"pin","query":"up","top":null}`},
		{"timeline", `{"namespace":"ns"}`}, {"timeline", `{"context":"","namespace":"ns"}`}, {"timeline", `{"context":"pin","namespace":"ns","repo":""}`},
		{"recall", `{"top":0}`}, {"run_resume", `{}`}, {"run_resume", `{"run_id":"run","offset":-1}`},
		{"run_resume", `{"run_id":"run","session_id":"session"}`}, {"report", `{"run_id":"run","out":"file"}`},
		{"ledger_add", `{"id":"e","kind":"evidence","payload":{"fact":"f","source":"s"}}`},
		{"ledger_add", `{"run_id":"run","id":"","kind":"evidence","payload":{"fact":"f","source":"s"}}`},
		{"ledger_add", `{"run_id":"run","id":"e","kind":"unknown","payload":{}}`},
		{"ledger_add", `{"run_id":"run","id":"e","kind":"evidence","payload":{"fact":"f","source":"s","extra":true}}`},
		{"ledger_add", `{"run_id":"run","id":"e","kind":"evidence","payload":{"fact":null,"source":"s"}}`},
		{"ledger_add", `{"run_id":"run","id":"e","kind":"metadata","payload":{}}`},
		{"ledger_add", `{"run_id":"run","id":"e","kind":"evidence","payload":{"fact":"a","fact":"b","source":"s"}}`},
	} {
		t.Run(tc.name+"/"+tc.input, func(t *testing.T) {
			if result := callTest(t, cs, tc.name, tc.input); !result.IsError {
				t.Fatalf("accepted invalid input: %+v", result)
			}
		})
	}
	if invoked.Load() != 0 {
		t.Fatalf("invalid inputs dispatched %d services", invoked.Load())
	}
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func TestDiscoveryDeadlineAndContext(t *testing.T) {
	var calls int
	cs := connectTest(t, newServer(dependencies{discover: func(ctx context.Context, opts discover.Options) (discover.Report, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 50*time.Second || time.Until(deadline) < 49*time.Second {
			t.Errorf("unbounded discovery deadline: %v", deadline)
		}
		if opts.Context != " exact pin " {
			t.Errorf("context changed: %q", opts.Context)
		}
		if calls == 1 && opts.Timeout != 0 {
			t.Errorf("changed service default timeout: %v", opts.Timeout)
		}
		if calls == 2 && opts.Timeout != 2*time.Minute {
			t.Errorf("changed probe timeout: %v", opts.Timeout)
		}
		return discover.Report{Probes: map[string]discover.ProbeResult{}}, nil
	}}))
	callTest(t, cs, "discover", `{"context":" exact pin "}`)
	callTest(t, cs, "discover", `{"context":" exact pin ","timeout":"2m"}`)
}

func TestRunCancellationClosesInput(t *testing.T) {
	input, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, input, io.Discard, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not close a blocked input on cancellation")
	}
}

func TestAdapterOptions(t *testing.T) {
	var pod logs.PodOptions
	var query prom.Options
	var changes timeline.Options
	cs := connectTest(t, newServer(dependencies{
		foldPod: func(_ context.Context, opts logs.PodOptions) (logs.Report, error) {
			pod = opts
			return logs.Report{}, nil
		},
		query: func(_ context.Context, opts prom.Options) (prom.Report, error) {
			query = opts
			return prom.Report{}, nil
		},
		collect: func(_ context.Context, opts timeline.Options) (timeline.Report, error) {
			changes = opts
			return timeline.Report{}, nil
		},
	}))
	for _, tc := range []struct{ name, input string }{
		{"logs_fold", `{"context":" exact pin ","pod":"api","namespace":"ns","container":"worker","previous":true,"since":"3m","max_patterns":7}`},
		{"prom_query", `{"context":" exact pin ","query":"up","url":"http://localhost:9090","range":"3m","step":"5s","top":7,"timeout":"2s"}`},
		{"timeline", `{"context":" exact pin ","namespace":"ns","workload":"deployment/api","repo":"path","alertmanager_url":"http://localhost:9093","since":"3m","timeout":"2s"}`},
	} {
		if r := callTest(t, cs, tc.name, tc.input); r.IsError {
			t.Fatalf("valid input rejected: %+v", r)
		}
	}
	if want := (logs.PodOptions{Context: " exact pin ", Pod: "api", Namespace: "ns", Container: "worker", Previous: true, Since: 3 * time.Minute, Options: logs.Options{MaxPatterns: 7}}); !reflect.DeepEqual(pod, want) {
		t.Fatalf("pod options=%+v", pod)
	}
	if want := (prom.Options{Context: " exact pin ", Query: "up", URL: "http://localhost:9090", Range: 3 * time.Minute, Step: 5 * time.Second, Top: 7, Timeout: 2 * time.Second}); !reflect.DeepEqual(query, want) {
		t.Fatalf("query options=%+v", query)
	}
	if want := (timeline.Options{Context: " exact pin ", Namespace: "ns", Workload: "deployment/api", Repo: "path", AlertmanagerURL: "http://localhost:9093", Since: 3 * time.Minute, Timeout: 2 * time.Second}); !reflect.DeepEqual(changes, want) {
		t.Fatalf("timeline options=%+v", changes)
	}
}
