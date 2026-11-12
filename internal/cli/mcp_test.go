package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/mcpserver"
	"github.com/glapsfun/sre-agent/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPRouteBypassesEnvelope(t *testing.T) {
	var out, diagnostics bytes.Buffer
	code := RunWithInput([]string{"mcp"}, strings.NewReader(""), &out, &diagnostics)
	if code != 0 || out.Len() != 0 {
		t.Fatalf("MCP EOF must exit without a CLI envelope: exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
	}
	for _, args := range [][]string{{"mcp", "--format=json"}, {"mcp", "extra"}, {"mcp", "--context", "pin"}} {
		out.Reset()
		diagnostics.Reset()
		if code := RunWithInput(args, strings.NewReader(""), &out, &diagnostics); code != 1 || out.Len() != 0 || diagnostics.Len() == 0 {
			t.Fatalf("invalid MCP invocation polluted stdout: exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
		}
	}
}

func TestDefaultClockAfterMCPSeam(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	before := time.Now().UTC()
	var out, diagnostics bytes.Buffer
	if code := RunWithInput([]string{"run", "start", "incident", "--id", "run", "--repo", repo}, strings.NewReader(""), &out, &diagnostics); code != 0 {
		t.Fatalf("start: %s %s", &out, &diagnostics)
	}
	out.Reset()
	if code := RunWithInput([]string{"ledger", "add", "evidence", "--run", "run", "--id", "entry", "--file", "-", "--repo", repo}, strings.NewReader(`{"fact":"f","source":"s"}`), &out, &diagnostics); code != 0 {
		t.Fatalf("add: %s %s", &out, &diagnostics)
	}
	after := time.Now().UTC()
	db, err := store.Open(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	run, err := ledger.New(db, nil).Load(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if run.CreatedAt.Before(before) || run.CreatedAt.After(after) || run.Entries[0].RecordedAt.Before(before) || run.Entries[0].RecordedAt.After(after) {
		t.Fatalf("production clock changed: %+v", run)
	}
}

func TestCLIParity(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) }
	a, b := mcp.NewInMemoryTransports()
	ss, err := mcpserver.New(mcpserver.Dependencies{Now: now}).Connect(t.Context(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "parity", Version: "1"}, nil).Connect(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	seed := func() string {
		repo := t.TempDir()
		if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
			t.Fatalf("init: %v %s", err, out)
		}
		db, err := store.Open(t.Context(), repo)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := ledger.New(db, now).Start(t.Context(), ledger.StartRequest{ID: "run", Problem: "Investigate errors"}); err != nil {
			t.Fatal(err)
		}
		return repo
	}
	cliRepo, mcpRepo := seed(), seed()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/alerts") {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"service":"api"},"values":[[1794484740,"2"],[1794484800,"5"]]}]}}`)
	}))
	defer server.Close()
	t.Setenv("SRE_SKIP_CLUSTER", "false")
	t.Setenv("SRE_ALERTMANAGER_URL", "")
	bin := t.TempDir()
	for name, script := range map[string]string{
		"aws":     "#!/bin/sh\necho 'AccessDenied token=hidden' >&2\nexit 1\n",
		"gh":      "#!/bin/sh\nprintf '%s\\n' '{\"nameWithOwner\":\"owner/repo\",\"url\":\"https://github.com/owner/repo\"}'\n",
		"kubectl": "#!/bin/sh\nprintf '%s\\n' '{\"apiVersion\":\"v1\",\"kind\":\"List\",\"items\":[]}'\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	payload := `{"fact":"errors increased token=hidden","source":"logs"}`
	for _, tc := range []struct {
		name  string
		args  []string
		input map[string]any
		stdin string
	}{
		{"discover", []string{"discover", "--context", "pin", "--only", "aws,github"}, map[string]any{"context": "pin", "only": []string{"aws", "github"}}, ""},
		{"logs_fold", []string{"logs", "fold", "--max-patterns", "2"}, map[string]any{"context": "pin", "text": "2026-11-12T11:59:00Z ERROR token=hidden\n2026-11-12T12:00:00Z ERROR token=hidden\n", "max_patterns": 2}, "2026-11-12T11:59:00Z ERROR token=hidden\n2026-11-12T12:00:00Z ERROR token=hidden\n"},
		{"prom_query", []string{"prom", "query", "up", "--context", "pin", "--url", server.URL, "--range", "2m", "--step", "30s"}, map[string]any{"context": "pin", "query": "up", "url": server.URL, "range": "2m", "step": "30s"}, ""},
		{"timeline", []string{"timeline", "--context", "pin", "--namespace", "ns", "--repo", cliRepo, "--alertmanager-url", server.URL}, map[string]any{"context": "pin", "namespace": "ns", "repo": cliRepo, "alertmanager_url": server.URL}, ""},
		{"recall", []string{"recall", "--repo", cliRepo, "--service", "api", "--symptom", "errors", "--fingerprint", "failures"}, map[string]any{"repo": mcpRepo, "service": "api", "symptom": "errors", "fingerprint": "failures"}, ""},
		{"run_resume", []string{"run", "resume", "run", "--repo", cliRepo}, map[string]any{"repo": mcpRepo, "run_id": "run"}, ""},
		{"ledger_add", []string{"ledger", "add", "evidence", "--run", "run", "--id", "entry", "--file", "-", "--repo", cliRepo}, map[string]any{"repo": mcpRepo, "run_id": "run", "id": "entry", "kind": "evidence", "payload": json.RawMessage(payload)}, payload},
		{"ledger_add", []string{"ledger", "add", "evidence", "--run", "run", "--id", "entry", "--file", "-", "--repo", cliRepo}, map[string]any{"repo": mcpRepo, "run_id": "run", "id": "entry", "kind": "evidence", "payload": json.RawMessage(payload)}, payload},
		{"report", []string{"report", "run", "--repo", cliRepo}, map[string]any{"repo": mcpRepo, "run_id": "run"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			code := runWithInputAt(tc.args, strings.NewReader(tc.stdin), &out, &diagnostics, func(string) (any, error) { return nil, nil }, now)
			result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.input})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError != (code != 0) {
				t.Fatalf("status differs: cli=%s mcp=%+v", &out, result)
			}
			var cliValue, mcpValue any
			if err := json.Unmarshal(out.Bytes(), &cliValue); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(result.StructuredContent)
			if err := json.Unmarshal(raw, &mcpValue); err != nil {
				t.Fatal(err)
			}
			// Only random safety tokens differ. Timestamps and every domain field remain exact.
			pattern := regexp.MustCompile(`((?:BEGIN|END) EXTERNAL DATA )[a-f0-9]{32}`)
			left, _ := json.Marshal(cliValue)
			right, _ := json.Marshal(mcpValue)
			if !reflect.DeepEqual(pattern.ReplaceAll(left, []byte("${1}TOKEN")), pattern.ReplaceAll(right, []byte("${1}TOKEN"))) {
				t.Fatalf("logical parity mismatch\nCLI %s\nMCP %s", left, right)
			}
			if diagnostics.Len() != 0 {
				t.Fatalf("CLI diagnostics: %s", &diagnostics)
			}
		})
	}
	for _, repo := range []string{cliRepo, mcpRepo} {
		db, err := store.Open(context.Background(), repo)
		if err != nil {
			t.Fatal(err)
		}
		run, err := ledger.New(db, nil).Load(t.Context(), "run")
		_ = db.Close()
		if err != nil || run.ThroughSeq != 1 {
			t.Fatalf("retry: %+v %v", run, err)
		}
	}
}
