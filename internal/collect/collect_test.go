package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/sources"
)

const fixtureSecret = "fixture-private-value"

type providerFixture struct {
	source   sources.Source
	contacts atomic.Int32
	calls    atomic.Int32
	fail     atomic.Bool
	payload  atomic.Value
	service  sources.Service
}

// These providers enforce a reader credential and expose only a read operation.
func newProvider(t *testing.T, root, id, kind string) *providerFixture {
	t.Helper()
	f := &providerFixture{}
	f.payload.Store(`{"fact":"observed"}`)
	var endpoint string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.contacts.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+fixtureSecret {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if kind == "utcp-http" {
			if r.Method != "GET" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if r.URL.Path == "/manual" {
				fmt.Fprintf(w, `{"utcp_version":"1.1.0","manual_version":"one","tools":[{"name":"read","inputs":{"type":"object","properties":{},"additionalProperties":false},"tool_call_template":{"call_template_type":"http","url":%q,"http_method":"GET","auth":{"auth_type":"api_key","api_key":"Bearer ${FIXTURE_TOKEN}","var_name":"Authorization","location":"header"}}}]}`, endpoint+"/read")
				return
			}
			if r.URL.Path != "/read" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			f.calls.Add(1)
			if f.fail.Load() {
				w.WriteHeader(503)
				return
			}
			fmt.Fprint(w, f.payload.Load().(string))
			return
		}
		if r.Method != "POST" {
			w.WriteHeader(204)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"1"}}}`, req.ID)
		case "notifications/initialized", "notifications/cancelled":
			w.WriteHeader(202)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"read","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}}]}}`, req.ID)
		case "tools/call":
			if req.Params.Name != "read" {
				w.WriteHeader(403)
				return
			}
			f.calls.Add(1)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[],"structuredContent":%s,"isError":%t}}`, req.ID, f.payload.Load().(string), f.fail.Load())
		default:
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(server.Close)
	endpoint = server.URL
	f.service = sources.Service{LookupEnv: func(key string) (string, bool) {
		if key != "FIXTURE_TOKEN" {
			t.Errorf("unexpected lookup %q", key)
		}
		return fixtureSecret, true
	}}
	f.source = sources.Source{ID: id, Name: id, Kind: kind, Endpoint: endpoint + "/manual", CredentialEnv: "FIXTURE_TOKEN", AccessRef: "provider-enforced-reader", Tools: []sources.Tool{}}
	known, _ := json.Marshal(map[string]any{"sources": []sources.Source{f.source}})
	if err := os.WriteFile(filepath.Join(root, "known-"+id+".json"), known, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := f.service.Scout(context.Background(), root, sources.ScoutOptions{Approved: true, KnownFile: "known-" + id + ".json"})
	if err != nil || len(report.Candidates) != 1 || len(report.Candidates[0].Tools) != 1 {
		t.Fatalf("scout: %+v %v", report, err)
	}
	f.source = report.Candidates[0].Source
	u, _ := url.Parse(endpoint)
	f.source.CodeNetwork = &sources.CodeNetwork{HostPort: u.Host, AccessRef: "reader-network"}
	approveFixture(t, root, f.source)
	f.contacts.Store(0)
	f.calls.Store(0)
	return f
}
func approveFixture(t *testing.T, root string, src sources.Source) {
	t.Helper()
	if _, err := sources.Approve(context.Background(), root, sources.ApprovalRequest{Approved: true, CodeNetworkApproved: src.CodeNetwork != nil, Sources: []sources.Source{src}}); err != nil {
		t.Fatal(err)
	}
}
func fixturePlan(ids ...string) Plan {
	p := Plan{Operation: "filter", Required: []string{}}
	for i, id := range ids {
		key := fmt.Sprintf("k%d", i)
		p.Calls = append(p.Calls, Call{Key: key, SourceID: id, Tool: "read", Arguments: json.RawMessage(`{}`)})
		p.Required = append(p.Required, key)
	}
	return p
}

// runtimeContractDouble tests host orchestration only. It executes no JavaScript
// and provides no evidence of sandbox isolation or real runtime readiness.
type runtimeContractDouble struct {
	probe                       func(context.Context) error
	open                        func(context.Context) (Session, error)
	run                         func(context.Context, Invocation, Helper) (Execution, error)
	close                       func(context.Context) error
	probes, opens, runs, closes int
}

func (r *runtimeContractDouble) Probe(ctx context.Context) error {
	r.probes++
	if r.probe != nil {
		return r.probe(ctx)
	}
	return nil
}
func (r *runtimeContractDouble) Open(ctx context.Context) (Session, error) {
	r.opens++
	if r.open != nil {
		return r.open(ctx)
	}
	return r, nil
}
func (r *runtimeContractDouble) Run(ctx context.Context, i Invocation, h Helper) (Execution, error) {
	r.runs++
	if r.run != nil {
		return r.run(ctx, i, h)
	}
	return Execution{}, nil
}
func (r *runtimeContractDouble) Close(ctx context.Context) error {
	r.closes++
	if r.close != nil {
		return r.close(ctx)
	}
	return nil
}
func helperValues(_ context.Context, i Invocation, h Helper) (Execution, error) {
	values := []Value{}
	for _, k := range i.Keys {
		r, _ := h(k.Key)
		if len(r.Data) > 0 {
			values = append(values, Value{Key: k.Key, SourceID: k.SourceID, Data: r.Data})
		}
	}
	return Execution{Values: values}, nil
}

func TestOrdinaryRealProviders(t *testing.T) {
	for _, kind := range []string{"mcp-http", "utcp-http"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			f := newProvider(t, root, "s", kind)
			runtime := &runtimeContractDouble{}
			result, err := (Service{Sources: f.service, Runtime: runtime}).Collect(context.Background(), root, Request{Plan: fixturePlan("s", "s"), Mode: ModeOrdinary, Program: "throw new Error('unused')", CodeApproved: true})
			if err != nil || result.SelectedMode != ModeOrdinary || !result.RequiredKeysSatisfied || len(result.Activity) != 2 || len(result.Evidence) != 2 || f.calls.Load() != 2 || runtime.probes != 0 || runtime.runs != 0 {
				t.Fatalf("ordinary: %+v %v calls=%d runtime=%+v", result, err, f.calls.Load(), runtime)
			}
			for i, e := range result.Evidence {
				if e.Key != fmt.Sprintf("k%d", i) || e.Origin != "source" || e.Tool != "read" || e.Reference != f.source.Endpoint || e.At.IsZero() {
					t.Fatalf("provenance: %+v", e)
				}
			}
			raw, _ := json.Marshal(result)
			for _, key := range []string{`"input_tokens":null`, `"output_tokens":null`, `"cost_usd":null`, `"audit":[]`, `"missing_required":[]`, `"gaps":[]`} {
				if !strings.Contains(string(raw), key) {
					t.Fatalf("missing stable field %s in %s", key, raw)
				}
			}
			if result.CodeSHA256 != "" || result.UsageReason == "" {
				t.Fatal("ordinary usage metadata")
			}
		})
	}
}

func TestSelectionRuntimeContractDouble(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   Mode
		prereq string
		want   Mode
		probe  bool
	}{
		{"manual", ModeOrdinary, "", ModeOrdinary, false}, {"short", ModeAuto, "short", ModeOrdinary, false}, {"independent", ModeAuto, "independent", ModeOrdinary, false},
		{"no program", ModeAuto, "program", ModeOrdinary, false}, {"no consent", ModeAuto, "consent", ModeOrdinary, false}, {"no grant", ModeAuto, "grant", ModeOrdinary, false}, {"no credential", ModeAuto, "credential", ModeOrdinary, false}, {"nil runtime", ModeAuto, "nil", ModeOrdinary, false}, {"probe failed", ModeAuto, "probe", ModeOrdinary, true},
		{"eligible", ModeAuto, "", ModeCode, true}, {"explicit", ModeCode, "", ModeCode, true}, {"explicit unavailable", ModeCode, "program", ModeNone, false},
		{"explicit no consent", ModeCode, "consent", ModeNone, false}, {"explicit no grant", ModeCode, "grant", ModeNone, false}, {"explicit nil runtime", ModeCode, "nil", ModeNone, false}, {"explicit probe failure", ModeCode, "probe", ModeNone, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			f := newProvider(t, root, "s", "utcp-http")
			r := &runtimeContractDouble{run: helperValues}
			req := Request{Plan: fixturePlan("s", "s"), Mode: tc.mode, Program: "return []", CodeApproved: true}
			switch tc.prereq {
			case "short":
				req.Plan = fixturePlan("s")
			case "independent":
				req.Plan.Operation = "independent"
			case "program":
				req.Program = ""
			case "consent":
				req.CodeApproved = false
			case "grant":
				f.source.CodeNetwork = nil
				approveFixture(t, root, f.source)
			case "credential":
				f.service.LookupEnv = func(string) (string, bool) { return "", false }
			case "probe":
				r.probe = func(context.Context) error { return errors.New("private probe diagnostic") }
			}
			s := Service{Sources: f.service, Runtime: r}
			if tc.prereq == "nil" {
				s.Runtime = nil
			}
			result, err := s.Collect(context.Background(), root, req)
			if result.SelectedMode != tc.want || (r.probes > 0) != tc.probe || result.Reason == "" {
				t.Fatalf("selection: %+v %v runtime=%+v", result, err, r)
			}
			if tc.want == ModeNone && (!errors.Is(err, ErrCodeUnavailable) || f.contacts.Load() != 0) {
				t.Fatalf("unavailable: %+v %v contacts=%d", result, err, f.contacts.Load())
			}
			if tc.want == ModeCode && (err != nil || r.opens != 1 || r.closes != 1 || len(result.CodeSHA256) != 64) {
				t.Fatalf("code: %+v %v runtime=%+v", result, err, r)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("raw runtime error leaked")
			}
		})
	}
}

func TestPreflightAllBeforeContact(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	for _, change := range []string{"missing source", "missing tool", "bad mode", "bad timeout", "bad program"} {
		t.Run(change, func(t *testing.T) {
			req := Request{Plan: fixturePlan("s", "s"), Mode: ModeOrdinary}
			switch change {
			case "missing source":
				req.Plan.Calls[1].SourceID = "absent"
			case "missing tool":
				req.Plan.Calls[1].Tool = "write"
			case "bad mode":
				req.Mode = ModeNone
			case "bad timeout":
				req.Timeout = 61 * time.Second
			case "bad program":
				req.Program = strings.Repeat("a", 65537)
			}
			result, err := (Service{Sources: f.service}).Collect(context.Background(), root, req)
			if !errors.Is(err, ErrInvalidPlan) || result.SelectedMode != ModeNone || f.contacts.Load() != 0 {
				t.Fatalf("preflight: %+v %v contacts=%d", result, err, f.contacts.Load())
			}
		})
	}
}

func TestOrdinaryFailureAndBudget(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprint(large), func(t *testing.T) {
			f.fail.Store(!large)
			if large {
				f.payload.Store(`{"data":"` + strings.Repeat("x", 65536) + `"}`)
			}
			result, err := (Service{Sources: f.service}).Collect(context.Background(), root, Request{Plan: fixturePlan("s", "s"), Mode: ModeOrdinary})
			if !errors.Is(err, ErrIncompleteCollection) || len(result.Activity) != 2 || len(result.Gaps) < 2 || len(result.MissingRequired) != 2 || len(result.Evidence) != 0 {
				t.Fatalf("failure/budget: %+v %v", result, err)
			}
		})
	}
}

func TestOrdinaryPartialFailureRetainsHostProvenance(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "mcp-http")
	f.fail.Store(true)
	result, err := (Service{Sources: f.service}).Collect(context.Background(), root, Request{Plan: fixturePlan("s"), Mode: ModeOrdinary})
	if !errors.Is(err, ErrIncompleteCollection) || len(result.Evidence) != 1 || result.Evidence[0].Tool != "read" || result.Evidence[0].Origin != "source" || result.Evidence[0].At.IsZero() || result.Evidence[0].Reference != f.source.Endpoint || result.RequiredKeysSatisfied {
		t.Fatalf("partial source receipt %+v %v", result, err)
	}
}
