package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/sources"
	"github.com/glapsfun/sre-agent/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func sourceCLI(t *testing.T, root, input string, args ...string) (int, Envelope, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	args = append([]string{"sources"}, args...)
	args = append(args, "--repo", root)
	code := RunWithInput(args, strings.NewReader(input), &out, &stderr)
	var envelope Envelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid output: %s %s", out.String(), stderr.String())
	}
	return code, envelope, out.String()
}

func TestSourcesCLIConsentAndMissingRegistryDoNotCreateState(t *testing.T) {
	root := ledgerRepo(t)
	code, env, raw := sourceCLI(t, root, "", "list")
	if code != 0 || !env.OK || !strings.Contains(raw, "needs_scout_approval") {
		t.Fatal(code, raw)
	}
	for _, args := range [][]string{{"scout"}, {"approve", "--file", "missing"}, {"list", "--approve"}, {"call", "x", "y", "--args", "-"}} {
		if code, _, raw := sourceCLI(t, root, "{}", args...); code == 0 {
			t.Fatal("accepted invalid or unapproved command", raw)
		}
	}
	for _, path := range []string{sources.RegistryFilename, ".sre-agent", ".sre-sources.lock"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatal("created state", path, err)
		}
	}
}

func TestSourcesCLIApprovalIsExplicitAndStrict(t *testing.T) {
	root := ledgerRepo(t)
	input := `{"sources":[{"id":"logs","name":"Logs","kind":"mcp-stdio","reference":".mcp.json logs","tools":[]}]}`
	for _, bad := range []string{`null`, `{}`, `{"sources":[],"unknown":true}`, `{"sources":[],"sources":[]}`, strings.Replace(input, `"name":"Logs"`, `"name":"Logs","name":"other"`, 1)} {
		if code, _, raw := sourceCLI(t, root, bad, "approve", "--approve", "--file", "-"); code == 0 {
			t.Fatal("accepted malformed approval", raw)
		}
	}
	if code, _, raw := sourceCLI(t, root, input, "approve", "--approve", "--file", "-"); code != 0 {
		t.Fatal(raw)
	}
	before, _ := os.ReadFile(filepath.Join(root, sources.RegistryFilename))
	if code, _, raw := sourceCLI(t, root, input, "approve", "--approve", "--file", "-"); code != 0 {
		t.Fatal(raw)
	}
	after, _ := os.ReadFile(filepath.Join(root, sources.RegistryFilename))
	if !bytes.Equal(before, after) {
		t.Fatal("approval replay changed registry")
	}
	if code, _, raw := sourceCLI(t, root, "", "list"); code != 0 || !strings.Contains(raw, "Logs") {
		t.Fatal(raw)
	}
	if _, err := os.Stat(filepath.Join(root, ".sre-agent")); !os.IsNotExist(err) {
		t.Fatal("source approval initialized ledger")
	}
}

func TestSourcesCLICallRecordsSuccessAndFailureGaps(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "incident", "--id", "run")
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "queue_depth=12"}}}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(httpServer.Close)
	known, _ := json.Marshal(map[string]any{"sources": []sources.Source{{ID: "metrics", Name: "Metrics", Kind: "mcp-http", Endpoint: httpServer.URL, AccessRef: "fixture read-only role", Tools: []sources.Tool{}}}})
	knownPath := filepath.Join(root, "known.json")
	if err := os.WriteFile(knownPath, known, 0600); err != nil {
		t.Fatal(err)
	}
	scout, err := (sources.Service{}).Scout(t.Context(), root, sources.ScoutOptions{Approved: true, KnownFile: knownPath})
	if err != nil || len(scout.Candidates) != 1 {
		t.Fatal(scout, err)
	}
	_, err = sources.Approve(t.Context(), root, sources.ApprovalRequest{Approved: true, Sources: []sources.Source{scout.Candidates[0].Source}})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`null`, `[]`, `{"x":1,"x":2}`, `{} {}`} {
		if code, _, _ := sourceCLI(t, root, bad, "call", "metrics", "read", "--run", "run", "--id", "invalid", "--args", "-"); code == 0 {
			t.Fatal("invalid arguments accepted")
		}
	}
	check, err := store.OpenReadOnly(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ledger.New(check, nil).Load(t.Context(), "run")
	check.Close()
	if err != nil || len(before.Entries) != 0 || calls.Load() != 0 {
		t.Fatal("invalid input caused collection or persistence", before, err)
	}
	if code, _, raw := sourceCLI(t, root, "{}", "call", "metrics", "read", "--run", "run", "--id", "success", "--args", "-"); code != 0 || !strings.Contains(raw, "queue_depth=12") || !strings.Contains(raw, `"committed":true`) {
		t.Fatal(code, raw)
	}
	if calls.Load() != 1 {
		t.Fatal("wrong invocation count", calls.Load())
	}
	if code, _, raw := sourceCLI(t, root, "{}", "call", "metrics", "read", "--run", "run", "--id", "success", "--args", "-"); code != 0 || !strings.Contains(raw, "replayed") {
		t.Fatal("lost retry receipt", raw)
	}
	if calls.Load() != 1 {
		t.Fatal("receipt retry repeated remote call")
	}
	var stderr bytes.Buffer
	if code := runSources([]string{"call", "metrics", "read", "--repo", root, "--run", "run", "--id", "success", "--args", "-"}, strings.NewReader("{}"), ledgerBrokenWriter{}, &stderr, nil); code != 1 || !strings.Contains(stderr.String(), "committed for run run entry success") {
		t.Fatal("output failure lost receipt", stderr.String())
	}
	clockCalls := 0
	result, gaps, err := executeSources(t.Context(), sourceOptions{command: "call", repo: root, source: "metrics", tool: "read", run: "run", id: "unrecorded", args: "-"}, strings.NewReader("{}"), sources.Service{Now: func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return time.Now()
		}
		return time.Time{}
	}})
	if err == nil || result.Committed || result.Result == nil || !strings.Contains(strings.Join(gaps, " "), "not recorded") || calls.Load() != 2 {
		t.Fatal("write failure hid returned evidence or repeated call", result, gaps, err)
	}
	httpServer.Close()
	if code, _, raw := sourceCLI(t, root, "{}", "call", "metrics", "read", "--run", "run", "--id", "failure", "--args", "-"); code == 0 || !strings.Contains(raw, `"committed":true`) {
		t.Fatal("failed source lacked gap receipt", raw)
	}
	db, err := store.OpenReadOnly(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	run, err := ledger.New(db, nil).Load(t.Context(), "run")
	if err != nil || len(run.State.Evidence) != 2 {
		t.Fatal(run, err)
	}
	value, err := ledger.DecodePayload(ledger.KindEvidence, run.State.Evidence[1].Payload)
	if err != nil || value.(ledger.Evidence).Gap == "" || strings.Contains(string(run.State.Evidence[0].Payload), "queue_depth") {
		t.Fatal("missing gap or raw response persisted", value, err)
	}
}
