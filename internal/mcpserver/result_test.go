package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/prom"
	"github.com/glapsfun/sre-agent/internal/store"
	"github.com/glapsfun/sre-agent/internal/timeline"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func resultJSON(t *testing.T, result *mcp.CallToolResult) []byte {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 8000 {
		t.Fatalf("encoded result exceeds limit: %d", len(raw))
	}
	if len(result.Content) != 1 {
		t.Fatalf("content=%+v", result.Content)
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("non-text result: %+v", result.Content)
	}
	var text, structured any
	if err := json.Unmarshal([]byte(content.Text), &text); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &structured); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(text)
	b, _ := json.Marshal(structured)
	if string(a) != string(b) {
		t.Fatalf("representations differ: %s / %s", a, b)
	}
	return raw
}

func boundedResult(env envelope, command string) *mcp.CallToolResult {
	encoded, _ := json.Marshal(toolResult(env))
	var result mcp.CallToolResult
	if err := json.Unmarshal(boundEncoded(encoded, command), &result); err != nil {
		panic(err)
	}
	return &result
}

func TestResultBudget(t *testing.T) {
	t.Run("SDK metadata at wire limit", func(t *testing.T) {
		cs := connectTest(t, newServer(dependencies{query: func(_ context.Context, opts prom.Options) (prom.Report, error) {
			n, err := strconv.Atoi(opts.Query)
			if err != nil {
				return prom.Report{}, err
			}
			return prom.Report{Infos: []string{strings.Repeat("x", n)}}, nil
		}}))
		base := callTest(t, cs, "prom_query", `{"context":"pin","query":"0"}`)
		encoded, _ := json.Marshal(base)
		n := (8000-len(encoded))/2 + 1
		input, _ := json.Marshal(map[string]any{"context": "pin", "query": strconv.Itoa(n)})
		result := callTest(t, cs, "prom_query", string(input))
		raw := resultJSON(t, result)
		var body struct {
			Data struct {
				Actual int `json:"actual_bytes"`
			} `json:"data"`
		}
		structured, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(structured, &body); err != nil {
			t.Fatal(err)
		}
		if !result.IsError || body.Data.Actual != len(encoded)+2*n {
			t.Fatalf("wrong final encoded size: %s want=%d", raw, len(encoded)+2*n)
		}
	})
	t.Run("last fitting byte count", func(t *testing.T) {
		for n := 3800; n < 4100; n++ {
			env := envelope{OK: true, Data: strings.Repeat("x", n), Gaps: []string{}, Errors: []string{}}
			body, _ := json.Marshal(env)
			encoded, _ := json.Marshal(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}, StructuredContent: env})
			if len(encoded) < 7999 {
				continue
			}
			for _, extra := range []string{"", "x"} {
				env.Data = strings.Repeat("x", n) + extra
				result := boundedResult(env, "sre report RUN_ID --format json")
				resultJSON(t, result)
				if result.IsError != (len(encoded)+2*len(extra) > 8000) {
					t.Fatalf("wrong decision at %d bytes", len(encoded)+2*len(extra))
				}
			}
			return
		}
		t.Fatal("no boundary fixture found")
	})
	for _, sample := range []string{"x", "界", "\"\\\n<>&"} {
		t.Run(sample, func(t *testing.T) {
			var sawFit, sawOversize bool
			for _, n := range []int{1, 200, 1200, 3800, 4100, 9000} {
				env := envelope{OK: true, Data: strings.Repeat(sample, n), Gaps: []string{}, Errors: []string{}}
				text, _ := json.Marshal(env)
				complete := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}, StructuredContent: env}
				encoded, _ := json.Marshal(complete)
				result := boundedResult(env, "sre timeline --context CONTEXT --namespace NAMESPACE")
				raw := resultJSON(t, result)
				if len(encoded) <= 8000 {
					sawFit = true
					if result.IsError || len(raw) != len(encoded) || result.StructuredContent.(map[string]any)["data"] != env.Data {
						t.Fatalf("changed fitting result: %s", raw)
					}
				} else {
					sawOversize = true
					var failure struct {
						Data struct {
							Code        string `json:"code"`
							ActualBytes int    `json:"actual_bytes"`
							Limit       int    `json:"limit"`
							CLI         string `json:"cli"`
						} `json:"data"`
					}
					body, _ := json.Marshal(result.StructuredContent)
					if err := json.Unmarshal(body, &failure); err != nil {
						t.Fatal(err)
					}
					if !result.IsError || failure.Data.Code != "result_too_large" || failure.Data.ActualBytes != len(encoded) || failure.Data.Limit != 8000 || !strings.HasPrefix(failure.Data.CLI, "sre timeline") {
						t.Fatalf("missing size diagnostics: %s", raw)
					}
				}
			}
			if !sawFit || !sawOversize {
				t.Fatal("fixture did not cross budget")
			}
		})
	}
	t.Run("report", func(t *testing.T) {
		repo := seededRepo(t, strings.Repeat("large incident ", 1000))
		cs := connectTest(t, New(Dependencies{}))
		input, _ := json.Marshal(map[string]any{"repo": repo, "run_id": "run"})
		result := callTest(t, cs, "report", string(input))
		raw := resultJSON(t, result)
		if !result.IsError || !strings.Contains(string(raw), "result_too_large") || strings.Contains(string(raw), "large incident") {
			t.Fatalf("report silently truncated: %s", raw)
		}
	})
	t.Run("timeline", func(t *testing.T) {
		cs := connectTest(t, newServer(dependencies{collect: func(context.Context, timeline.Options) (timeline.Report, error) {
			return timeline.Report{Entries: []timeline.Entry{{Change: strings.Repeat("界\"", 3000)}}, Gaps: []timeline.Gap{}, Errors: []string{}}, nil
		}}))
		result := callTest(t, cs, "timeline", `{"context":"pin","namespace":"ns"}`)
		raw := resultJSON(t, result)
		if !result.IsError || !strings.Contains(string(raw), "result_too_large") || strings.Contains(string(raw), "entries") {
			t.Fatalf("timeline omitted without error: %s", raw)
		}
	})
	t.Run("committed retry", func(t *testing.T) {
		repo := seededRepo(t, "incident")
		cs := connectTest(t, New(Dependencies{}))
		input, _ := json.Marshal(map[string]any{"repo": repo, "run_id": "run", "id": "entry", "kind": "evidence", "payload": map[string]any{"fact": strings.Repeat("界\"\\", 4000), "source": "logs"}})
		for i := 0; i < 2; i++ {
			result := callTest(t, cs, "ledger_add", string(input))
			raw := resultJSON(t, result)
			var receipt struct {
				OK   bool `json:"ok"`
				Data struct {
					Committed bool   `json:"committed"`
					EntryID   string `json:"entry_id"`
					Seq       int64  `json:"seq"`
					View      any    `json:"view"`
				} `json:"data"`
			}
			body, _ := json.Marshal(result.StructuredContent)
			if err := json.Unmarshal(body, &receipt); err != nil {
				t.Fatal(err)
			}
			if result.IsError || !receipt.OK || !receipt.Data.Committed || receipt.Data.EntryID != "entry" || receipt.Data.Seq != 1 || receipt.Data.View != nil || !strings.Contains(string(raw), "result_too_large") {
				t.Fatalf("ambiguous committed result: %s", raw)
			}
		}
		db, err := store.Open(t.Context(), repo)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		run, err := ledger.New(db, nil).Load(t.Context(), "run")
		if err != nil {
			t.Fatal(err)
		}
		if len(run.Entries) != 1 || run.Entries[0].ID != "entry" || run.ThroughSeq != 1 {
			t.Fatalf("retry appended: %+v", run)
		}
	})
}

func seededRepo(t *testing.T, problem string) string {
	t.Helper()
	repo := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	db, err := store.Open(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = ledger.New(db, func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) }).Start(t.Context(), ledger.StartRequest{ID: "run", Problem: problem})
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestSecretsAndBoundaries(t *testing.T) {
	cs := connectTest(t, newServer(dependencies{query: func(context.Context, prom.Options) (prom.Report, error) {
		return prom.Report{}, errors.New("upstream token=planted-secret")
	}}))
	for _, tc := range []struct {
		name, input string
		fail        bool
	}{
		{"logs_fold", `{"context":"pin","text":"ERROR token=planted-secret"}`, false},
		{"prom_query", `{"context":"pin","query":"up"}`, true},
		{"prom_query", `{"context":"pin","query":"up","planted-secret":true}`, true},
	} {
		result := callTest(t, cs, tc.name, tc.input)
		raw := resultJSON(t, result)
		if result.IsError != tc.fail || strings.Contains(string(raw), "planted-secret") {
			t.Fatalf("unsafe result: %s", raw)
		}
		if !strings.Contains(string(raw), "BEGIN EXTERNAL DATA") {
			t.Fatalf("missing external boundary: %s", raw)
		}
	}
}

func TestTypedLedgerPayloadsAndResume(t *testing.T) {
	repo := seededRepo(t, "incident")
	cs := connectTest(t, New(Dependencies{Now: func() time.Time { return time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC) }}))
	fixtures := []struct{ id, kind, payload string }{
		{"m", "metadata", `{"phase":1,"environment":{"context":"pin","source_repos":[{"path":"/src","remote":"origin"}]},"started_at":"2026-11-12T00:00:00Z"}`},
		{"e", "evidence", `{"fact":"observed","source":"logs","observed_at":"2026-11-12T00:01:00Z"}`},
		{"h", "hypothesis", `{"statement":"cause","mechanism":"mechanism","confidence":"high","supporting":["e"],"contradicting":[],"discriminating_test":"compare","status":"open"}`},
		{"x", "expected-behavior", `{"criteria":[{"id":"healthy","description":"recovery","observation":"error rate","threshold":"low","source":"metrics"}]}`},
		{"o", "option", `{"summary":"restore","steps":["restore"],"risk":"rollout","pros":[],"cons":[],"expected_impact":"recovery","rollback":"revert","hypothesis_ids":["h"]}`},
		{"p", "approval", `{"option_id":"o","decision":"approved","statement":"approved","at":"2026-11-12T00:02:00Z","target":"pin"}`},
		{"a", "action", `{"at":"2026-11-12T00:03:00Z","command":"restore","result":"complete","effect":"remediation","status":"applied","option_id":"o","approval_id":"p","evidence_ids":["e"]}`},
		{"v", "validation", `{"expected_entry_id":"x","criterion_id":"healthy","action_id":"a","verdict":"pending","observed":"waiting","source":"metrics","evidence_ids":[],"at":"2026-11-12T00:04:00Z","reason":"window incomplete"}`},
		{"s", "safety-violation", `{"wave":1,"investigators":["logs"],"changes":[{"object":"Deployment/api","field":"spec.replicas","before_hash":"before","after_hash":"after","change":"changed"}]}`},
	}
	for _, f := range fixtures {
		input, _ := json.Marshal(map[string]any{"repo": repo, "run_id": "run", "id": f.id, "kind": f.kind, "payload": json.RawMessage(f.payload)})
		if r := callTest(t, cs, "ledger_add", string(input)); r.IsError {
			raw, _ := json.Marshal(r)
			t.Fatalf("valid %s rejected: %s", f.kind, raw)
		}
	}
	for _, payload := range []map[string]any{
		{"id": "e", "kind": "evidence", "payload": map[string]any{"fact": "different", "source": "logs"}},
		{"id": "bad", "kind": "hypothesis", "payload": json.RawMessage(`{"statement":"cause","mechanism":"mechanism","confidence":"high","supporting":["missing"],"contradicting":[],"discriminating_test":"compare","status":"open"}`)},
	} {
		payload["repo"] = repo
		payload["run_id"] = "run"
		input, _ := json.Marshal(payload)
		if !callTest(t, cs, "ledger_add", string(input)).IsError {
			t.Fatal("bypassed ledger conflict/reference validation")
		}
	}
	input, _ := json.Marshal(map[string]any{"repo": repo, "run_id": "run", "offset": 8})
	if r := callTest(t, cs, "run_resume", string(input)); r.IsError {
		t.Fatalf("resume failed: %+v", r)
	}
	db, err := store.Open(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	run, err := ledger.New(db, nil).Load(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if run.ThroughSeq != 9 || len(run.Entries) != 9 {
		t.Fatalf("writes changed history unexpectedly: %+v", run)
	}
	var sessions int
	if err := db.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM run_sessions").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("resume bound %d sessions", sessions)
	}
}
