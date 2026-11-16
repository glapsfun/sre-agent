package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

const codeNetworkSelection = `{"sources":[{"id":"metrics","name":"Metrics","kind":"mcp-http","endpoint":"https://MeTrIcS.Example/mcp","credential_env":"METRICS_TOKEN","access_ref":"policy/metrics-reader","tools":[],"code_network":{"host_port":"metrics.example:443","access_ref":"policy/read-only and no anonymous writes"}}]}`

func TestSourcesCLICodeNetworkConsentBeforeRootResolution(t *testing.T) {
	ordinary := strings.Replace(codeNetworkSelection, `,"code_network":{"host_port":"metrics.example:443","access_ref":"policy/read-only and no anonymous writes"}`, "", 1)
	for _, tt := range []struct {
		name, input, diagnostic string
		flags                   []string
	}{
		{"neither consent", codeNetworkSelection, sources.ErrApprovalRequired.Error(), nil},
		{"ordinary absent", codeNetworkSelection, sources.ErrApprovalRequired.Error(), []string{"--approve-code-network"}},
		{"ordinary false", codeNetworkSelection, sources.ErrApprovalRequired.Error(), []string{"--approve=false", "--approve-code-network"}},
		{"network absent", codeNetworkSelection, sources.ErrCodeNetworkApprovalRequired.Error(), []string{"--approve"}},
		{"network false", codeNetworkSelection, sources.ErrCodeNetworkApprovalRequired.Error(), []string{"--approve", "--approve-code-network=false"}},
		{"flag without grant", ordinary, "code network approval requires a selected code network grant", []string{"--approve", "--approve-code-network"}},
		{"null with flag", strings.Replace(codeNetworkSelection, `{"host_port":"metrics.example:443","access_ref":"policy/read-only and no anonymous writes"}`, "null", 1), "code network approval requires a selected code network grant", []string{"--approve", "--approve-code-network"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "missing")
			args := append([]string{"approve", "--file", "-"}, tt.flags...)
			code, env, raw := sourceCLI(t, root, tt.input, args...)
			if code == 0 || len(env.Errors) != 1 || env.Errors[0] != tt.diagnostic {
				t.Fatalf("root resolution preceded consent: %s", raw)
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatal("created state before consent", entries, err)
			}
		})
	}
	root := ledgerRepo(t)
	if err := os.Mkdir(filepath.Join(root, sources.RegistryFilename), 0700); err != nil {
		t.Fatal(err)
	}
	if code, env, raw := sourceCLI(t, root, codeNetworkSelection, "approve", "--approve", "--file", "-"); code == 0 || len(env.Errors) != 1 || env.Errors[0] != sources.ErrCodeNetworkApprovalRequired.Error() {
		t.Fatalf("unreadable registry preceded consent: %s", raw)
	}
}

func TestSourcesCLICodeNetworkFlagScope(t *testing.T) {
	for _, args := range [][]string{
		{"list", "--approve-code-network"},
		{"list", "--approve-code-network=false"},
		{"scout", "--approve-scout", "--approve-code-network"},
		{"call", "metrics", "query", "--args", "-", "--run", "run", "--id", "entry", "--approve-code-network"},
		{"approve", "--file", "-", "--approve", "--approve-code-network", "--approve-code-network=false"},
		{"approve", "--file", "-", "--approve", "--approve-code-network=invalid"},
	} {
		if _, err := parseSources(args); err == nil {
			t.Fatalf("accepted invalid flag usage: %v", args)
		}
	}
}

func TestSourcesCLICodeNetworkSavedReplayAndRemoval(t *testing.T) {
	root := ledgerRepo(t)
	path := filepath.Join(root, sources.RegistryFilename)
	approve := func(input string, flags ...string) sources.Registry {
		t.Helper()
		args := append([]string{"approve", "--approve", "--file", "-"}, flags...)
		if code, _, raw := sourceCLI(t, root, input, args...); code != 0 || !strings.Contains(raw, `"saved":true`) {
			t.Fatal("approval failed", raw)
		}
		loaded, err := sources.Load(t.Context(), root)
		if err != nil || len(loaded.Sources) != 1 {
			t.Fatal("registry not saved", loaded, err)
		}
		return loaded
	}
	first := approve(codeNetworkSelection, "--approve-code-network")
	got := first.Sources[0]
	if got.ID != "metrics" || got.Endpoint != "https://MeTrIcS.Example/mcp" || got.CredentialEnv != "METRICS_TOKEN" || got.AccessRef != "policy/metrics-reader" || got.CodeNetwork == nil || got.CodeNetwork.HostPort != "metrics.example:443" || got.CodeNetwork.AccessRef != "policy/read-only and no anonymous writes" {
		t.Fatalf("wrong saved descriptor: %+v", got)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, raw := sourceCLI(t, root, codeNetworkSelection, "approve", "--approve", "--file", "-"); code == 0 {
		t.Fatal("replay lacked special consent", raw)
	}
	replay := approve(codeNetworkSelection, "--approve-code-network=true")
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || !first.Sources[0].ApprovedAt.Equal(replay.Sources[0].ApprovedAt) {
		t.Fatal("replay rewrote registry", err)
	}
	code, _, raw := sourceCLI(t, root, "", "list")
	var listed struct {
		Data struct {
			Result struct {
				Registry sources.Registry `json:"registry"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &listed); err != nil || code != 0 || len(listed.Data.Result.Registry.Sources) != 1 || listed.Data.Result.Registry.Sources[0].CodeNetwork == nil || *listed.Data.Result.Registry.Sources[0].CodeNetwork != *got.CodeNetwork {
		t.Fatal("list lost saved grant", raw, err)
	}
	for _, replacement := range []string{
		strings.Replace(codeNetworkSelection, `,"code_network":{"host_port":"metrics.example:443","access_ref":"policy/read-only and no anonymous writes"}`, "", 1),
		strings.Replace(codeNetworkSelection, `{"host_port":"metrics.example:443","access_ref":"policy/read-only and no anonymous writes"}`, "null", 1),
	} {
		approve(codeNetworkSelection, "--approve-code-network")
		removed := approve(replacement, "--approve-code-network=false")
		if removed.Sources[0].CodeNetwork != nil {
			t.Fatal("ordinary replacement retained saved grant")
		}
	}
	inputPath := filepath.Join(t.TempDir(), "selected.json")
	if err := os.WriteFile(inputPath, []byte(codeNetworkSelection), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, raw := sourceCLI(t, root, "unread stdin", "approve", "--approve", "--approve-code-network", "--file", inputPath); code != 0 {
		t.Fatal("explicit file approval failed", raw)
	}
	loaded, err := sources.Load(t.Context(), root)
	if err != nil || len(loaded.Sources) != 1 || loaded.Sources[0].CodeNetwork == nil || *loaded.Sources[0].CodeNetwork != *got.CodeNetwork {
		t.Fatal("file input saved wrong grant", loaded, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".sre-agent")); !os.IsNotExist(err) {
		t.Fatal("approval initialized ledger", err)
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
	for _, format := range []string{"json", "text"} {
		receipt, gaps, err := executeSources(t.Context(), sourceOptions{command: "call", repo: root, source: "metrics", tool: "read", run: "run", id: "success", args: "-"}, strings.NewReader("{}"), sources.Service{})
		if err != nil {
			t.Fatal(err)
		}
		gaps = append(gaps, strings.Repeat("x", 2<<20))
		var output bytes.Buffer
		if code := presentSources(format, receipt, gaps, nil, &output, &stderr); code != 1 || output.Len() > 2<<20 {
			t.Fatal("receipt presentation did not fail within budget", code, output.Len())
		}
		var presented struct {
			Data sourceOutput `json:"data"`
		}
		if err := json.Unmarshal(output.Bytes(), &presented); err != nil {
			t.Fatal(err)
		}
		if !presented.Data.Committed || !presented.Data.Replayed || presented.Data.RunID != "run" || presented.Data.EntryID != "success" || presented.Data.Seq != 1 {
			t.Fatal("durable receipt identity lost", presented.Data)
		}
		if code, _, raw := sourceCLI(t, root, "{}", "call", "metrics", "read", "--run", presented.Data.RunID, "--id", presented.Data.EntryID, "--args", "-"); code != 0 || !strings.Contains(raw, `"replayed":true`) || calls.Load() != 1 {
			t.Fatal("presentation failure lost replay or repeated collection", code, calls.Load(), raw)
		}
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

func TestSourcesCLIScoutOutputBudget(t *testing.T) {
	root := ledgerRepo(t)
	var config strings.Builder
	config.WriteString(`{"mcpServers":{`)
	for i := 0; i < 35000; i++ {
		if i != 0 {
			config.WriteByte(',')
		}
		fmt.Fprintf(&config, `"s%d":{"command":"x"}`, i)
	}
	config.WriteString(`}}`)
	if config.Len() > 1<<20 {
		t.Fatal("fixture exceeds scout input limit")
	}
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(config.String()), 0600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			code, env, raw := sourceCLI(t, root, "", "scout", "--approve-scout", "--format", format)
			if code != 1 || env.OK || len(raw) > 2<<20 || len(env.Errors) == 0 {
				t.Fatalf("unbounded scout presentation: code=%d ok=%v bytes=%d gaps=%d errors=%v", code, env.OK, len(raw), len(env.Gaps), env.Errors)
			}
		})
	}
}

func TestSourcesCLIPartialScoutRemainsSuccessful(t *testing.T) {
	root := ledgerRepo(t)
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"logs":{"command":"x"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"json", "text"} {
		code, env, raw := sourceCLI(t, root, "", "scout", "--approve-scout", "--format", format)
		if code != 0 || !env.OK || len(env.Gaps) == 0 || !strings.Contains(raw, `"candidates"`) {
			t.Fatal("partial scout changed semantics", code, raw)
		}
	}
}

func TestSourcesPresentationSavedApprovalRecovery(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			root := ledgerRepo(t)
			input := `{"sources":[{"id":"logs","name":"Logs","kind":"mcp-stdio","reference":".mcp.json logs","tools":[]}]}`
			saved, gaps, err := executeSources(t.Context(), sourceOptions{command: "approve", repo: root, approve: true, file: "-"}, strings.NewReader(input), sources.Service{})
			if err != nil || !saved.Saved {
				t.Fatal("approval not saved", err)
			}
			var stdout, stderr bytes.Buffer
			if code := presentSources(format, saved, append(gaps, strings.Repeat("x", 2<<20)), nil, &stdout, &stderr); code != 1 || stdout.Len() > 2<<20 {
				t.Fatal("approval presentation did not fail within budget", code, stdout.Len())
			}
			var presented struct {
				Data sourceOutput `json:"data"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &presented); err != nil || !presented.Data.Saved {
				t.Fatal("saved approval metadata lost", err)
			}
			registry, err := sources.Load(t.Context(), root)
			if err != nil || len(registry.Sources) != 1 || registry.Sources[0].ID != "logs" {
				t.Fatal("saved approval not recoverable", registry, err)
			}
			if code, _, raw := sourceCLI(t, root, "", "list"); code != 0 || !strings.Contains(raw, "Logs") {
				t.Fatal("saved approval not listed", code, raw)
			}
		})
	}
}

func TestSourcesPresentationBudgetPreservesMetadata(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		for _, kind := range []string{"result", "gaps", "encoding", "newline", "indentation"} {
			t.Run(format+"/"+kind, func(t *testing.T) {
				output := &sourceOutput{Saved: true, Committed: true, Replayed: true, RunID: "run", EntryID: "entry", Seq: 3}
				gaps := []string{}
				switch kind {
				case "result":
					output.Result = strings.Repeat("x", 2<<20)
				case "gaps":
					gaps = []string{strings.Repeat("x", 2<<20)}
				case "encoding":
					output.Result = json.RawMessage(`{`)
				case "newline":
					gaps = []string{""}
					env := Envelope{OK: true, Data: output, Gaps: gaps, Errors: []string{}}
					raw, _ := json.Marshal(env)
					if format == "text" {
						raw, _ = json.MarshalIndent(env, "", "  ")
					}
					gaps[0] = strings.Repeat("x", (2<<20)-len(raw))
				case "indentation":
					output.Result = make([]int, 300000)
					compact, _ := json.Marshal(output.Result)
					indented, _ := json.MarshalIndent(output.Result, "", "  ")
					// Envelope nesting adds four spaces per item in text format.
					if len(compact) >= 2<<20 || len(indented)+4*300000 <= 2<<20 {
						t.Fatal("invalid indentation fixture")
					}
				}
				var stdout, stderr bytes.Buffer
				code := presentSources(format, output, gaps, nil, &stdout, &stderr)
				wantFailure := kind != "indentation" || format == "text"
				if !wantFailure {
					if code != 0 {
						t.Fatalf("compact result should fit: code=%d", code)
					}
					return
				}
				var got struct {
					OK     bool         `json:"ok"`
					Data   sourceOutput `json:"data"`
					Errors []string     `json:"errors"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if code != 1 || got.OK || stdout.Len() > 2<<20 || len(got.Errors) == 0 || got.Data.Result != nil || got.Data.Begin != "" || got.Data.Finish != "" {
					t.Fatalf("presentation failure not bounded: code=%d ok=%v bytes=%d result_present=%v errors=%v", code, got.OK, stdout.Len(), got.Data.Result != nil, got.Errors)
				}
				if !got.Data.Saved || !got.Data.Committed || !got.Data.Replayed || got.Data.RunID != "run" || got.Data.EntryID != "entry" || got.Data.Seq != 3 {
					t.Fatal("durable receipt metadata lost", got.Data)
				}
			})
		}
	}
}
