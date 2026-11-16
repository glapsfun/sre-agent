package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/collect"
	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/sources"
	"github.com/glapsfun/sre-agent/internal/store"
)

const collectPlanJSON = `{"calls":[{"key":"metric","source_id":"metrics","tool":"read"}],"required":["metric"],"operation":"independent"}`

func TestCollectRoutesAndRequiresExistingRun(t *testing.T) {
	root := ledgerRepo(t)
	var out, stderr bytes.Buffer
	code := RunWithInput([]string{"collect", "--repo", root, "--plan", "-", "--run", "run", "--id", "entry"}, strings.NewReader(collectPlanJSON), &out, &stderr)
	if code != 1 || !strings.Contains(out.String(), "existing collection run unavailable") {
		t.Fatalf("collection did not reach read-only preflight: %d %s %s", code, &out, &stderr)
	}
	if _, err := os.Stat(filepath.Join(root, ".sre-agent")); !os.IsNotExist(err) {
		t.Fatalf("preflight created state: %v", err)
	}
}

func TestCollectParseAndInputPreflight(t *testing.T) {
	base := []string{"--plan", "-", "--run", "run", "--id", "entry"}
	for _, extra := range [][]string{{"--wat"}, {"positional"}, {"--id", "other"}, {"--approve-code=invalid"}, {"--mode", "none"}, {"--timeout", "0s"}, {"--timeout", "61s"}, {"--timeout", "bad"}, {"--program", "-"}, {"--format", "yaml"}} {
		if _, err := parseCollect(append(append([]string{}, base...), extra...)); err == nil {
			t.Fatalf("accepted %v", extra)
		}
	}
	for _, args := range [][]string{nil, {"--plan", "-", "--run", "run"}, {"--plan", "-", "--run", "../bad", "--id", "e"}, {"--plan", "-", "--run", "r", "--id", strings.Repeat("e", 129)}, {"--plan"}} {
		if _, err := parseCollect(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	opts, err := parseCollect(base)
	if err != nil || opts.timeout != 10*time.Second || opts.mode != collect.ModeAuto || opts.format != "json" {
		t.Fatal(opts, err)
	}
	for _, input := range []string{`{}`, strings.Replace(collectPlanJSON, `"independent"`, `"unknown"`, 1), strings.Replace(collectPlanJSON, `"tool":"read"`, `"tool":"read","extra":true`, 1), strings.Repeat(" ", 64<<10) + collectPlanJSON, string([]byte{0xff})} {
		root := ledgerRepo(t)
		opts.repo = root
		out, _, err := executeCollect(t.Context(), opts, strings.NewReader(input), collect.Service{Runtime: forbiddenCollectRuntime{t}})
		if err == nil || out.Committed {
			t.Fatal("invalid input accepted", out, err)
		}
		if _, err := os.Stat(filepath.Join(root, ".sre-agent")); !os.IsNotExist(err) {
			t.Fatal("invalid input created store", err)
		}
	}
	root := ledgerRepo(t)
	opts.repo = root
	for _, raw := range [][]byte{{0xff}, bytes.Repeat([]byte("a"), (64<<10)+1)} {
		path := filepath.Join(root, "program.js")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		opts.program = path
		if _, _, err := executeCollect(t.Context(), opts, strings.NewReader(collectPlanJSON), collect.Service{}); err == nil || !strings.Contains(err.Error(), "program") {
			t.Fatal(err)
		}
	}
	opts.program = ""
	for _, path := range []string{filepath.Join(root, "missing"), root, filepath.Join(root, "link")} {
		if strings.HasSuffix(path, "link") {
			if err := os.Symlink(filepath.Join(root, "program.js"), path); err != nil {
				t.Fatal(err)
			}
		}
		opts.plan = path
		if _, _, err := executeCollect(t.Context(), opts, nil, collect.Service{}); err == nil {
			t.Fatal("accepted nonregular plan", path)
		}
	}
}

type forbiddenCollectRuntime struct{ t *testing.T }

func (r forbiddenCollectRuntime) Probe(context.Context) error {
	r.t.Error("unexpected runtime probe")
	return errors.New("unavailable")
}
func (r forbiddenCollectRuntime) Open(context.Context) (collect.Session, error) {
	r.t.Error("unexpected runtime open")
	return nil, errors.New("unavailable")
}

func collectionFixture(t *testing.T) (string, *sourceFixtureMeasurements, string) {
	t.Helper()
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "collection fixture", "--id", "run")
	measure := &sourceFixtureMeasurements{requests: map[string][]*measuredSourceRequest{}, calls: map[string]int{}}
	base, known := utcpComparisonServer(t, measure)
	input, _ := json.Marshal(map[string]any{"sources": known})
	path := filepath.Join(root, "known.json")
	if err := os.WriteFile(path, input, 0600); err != nil {
		t.Fatal(err)
	}
	scout, err := (sources.Service{}).Scout(t.Context(), root, sources.ScoutOptions{Approved: true, KnownFile: path})
	if err != nil || len(scout.Candidates) != 2 {
		t.Fatal(scout, err)
	}
	selected := []sources.Source{}
	for _, candidate := range scout.Candidates {
		source := candidate.Source
		endpoint, _ := url.Parse(source.Endpoint)
		source.CodeNetwork = &sources.CodeNetwork{HostPort: endpoint.Host, AccessRef: "fixture read-only code role"}
		selected = append(selected, source)
	}
	if _, err := sources.Approve(t.Context(), root, sources.ApprovalRequest{Approved: true, CodeNetworkApproved: true, Sources: selected}); err != nil {
		t.Fatal(err)
	}
	return root, measure, base
}
func collectionPlan(query string) string {
	return `{"calls":[{"key":"mcp","source_id":"mcp-metrics","tool":"query","arguments":{"query":"` + query + `"}},{"key":"native","source_id":"utcp-metrics","tool":"query","arguments":{"query":"` + query + `"}}],"required":["mcp","native"],"operation":"independent"}`
}
func collectionCLI(t *testing.T, root, input, id string, service collect.Service, extra ...string) (int, collectionOutput, []string) {
	t.Helper()
	var out, stderr bytes.Buffer
	args := append([]string{"--repo", root, "--plan", "-", "--run", "run", "--id", id}, extra...)
	code := runCollectWithService(args, strings.NewReader(input), &out, &stderr, service)
	var envelope struct {
		OK     bool             `json:"ok"`
		Data   collectionOutput `json:"data"`
		Gaps   []string         `json:"gaps"`
		Errors []string         `json:"errors"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid collection output: %v %s %s", err, &out, &stderr)
	}
	if envelope.OK != (code == 0) {
		t.Fatal("exit disagrees", code, &out)
	}
	return code, envelope.Data, envelope.Gaps
}
func TestCollectLiveReplayAndModes(t *testing.T) {
	root, measure, base := collectionFixture(t)
	program := filepath.Join(root, "program.js")
	if err := os.WriteFile(program, []byte("private-script-body"), 0600); err != nil {
		t.Fatal(err)
	}
	service := collect.Service{Runtime: forbiddenCollectRuntime{t}}
	for _, mode := range []string{"auto", "ordinary"} {
		code, out, _ := collectionCLI(t, root, collectionPlan("queue_depth"), mode, service, "--mode", mode, "--program", program)
		if code != 0 || !out.Committed || out.Replayed || out.Result == nil || out.Receipt == nil || !out.Receipt.Success || !out.Receipt.RequiredKeysSatisfied || out.Begin == "" || out.Finish == "" {
			t.Fatalf("live result missing: %d %+v", code, out)
		}
		if out.Result.SelectedMode != collect.ModeOrdinary || len(out.Result.Evidence) != 2 || out.Result.Evidence[0].Reference != base+"/mcp" || out.Result.Evidence[1].Reference != base+"/utcp" {
			t.Fatal(out.Result)
		}
		if out.Result.InputTokens != nil || out.Result.OutputTokens != nil || out.Result.CostUSD != nil || out.Result.UsageReason == "" {
			t.Fatal("invented usage", out.Result)
		}
	}
	code, out, _ := collectionCLI(t, root, collectionPlan("queue_depth"), "auto", service, "--timeout", "10s", "--program", program)
	if code != 0 || !out.Replayed || out.Result != nil || out.Begin != "" {
		t.Fatal("default normalization did not replay", out)
	}
	for _, extra := range [][]string{{"--mode", "ordinary", "--program", program}, {"--timeout", "9s", "--program", program}, {"--approve-code", "--program", program}, nil} {
		code, out, _ := collectionCLI(t, root, collectionPlan("queue_depth"), "auto", service, extra...)
		if code == 0 || out.Committed || out.Result != nil {
			t.Fatal("changed request replayed", extra, out)
		}
	}
	if code, out, _ := collectionCLI(t, root, collectionPlan("http_errors_total"), "auto", service, "--program", program); code == 0 || out.Result != nil {
		t.Fatal("changed plan replayed", out)
	}
	for _, id := range []string{"mcp-metrics", "utcp-metrics"} {
		_, calls := measure.snapshot(id, "queue_depth")
		if calls != 2 {
			t.Fatal("replay/conflict contacted source", id, calls)
		}
	}
	for _, mode := range []string{"code", "auto"} {
		input := strings.Replace(collectionPlan("queue_depth"), "independent", "join", 1)
		code, out, _ := collectionCLI(t, root, input, "runtime-"+mode, collect.Service{}, "--mode", mode, "--program", program, "--approve-code")
		if out.Receipt == nil || out.Receipt.Reason != "code runtime unavailable" || !out.Committed {
			t.Fatal("runtime status missing", out)
		}
		if mode == "code" && (code == 0 || out.Receipt.SelectedMode != collect.ModeNone) || mode == "auto" && (code != 0 || out.Receipt.SelectedMode != collect.ModeOrdinary) {
			t.Fatal(code, out)
		}
	}
	code, out, _ = collectionCLI(t, root, collectionPlan("unavailable"), "failure", service)
	if code == 0 || !out.Committed || out.Receipt.Success || out.Receipt.RequiredKeysSatisfied || len(out.Result.Evidence) == 0 {
		t.Fatal("failure lost partial result", out)
	}
	if err := os.Remove(filepath.Join(root, ".sre-sources.json")); err != nil {
		t.Fatal(err)
	}
	noContact := collect.Service{Runtime: forbiddenCollectRuntime{t}, Sources: sources.Service{LookupEnv: func(string) (string, bool) { t.Error("replay looked up credentials"); return "", false }}}
	code, out, gaps := collectionCLI(t, root, collectionPlan("unavailable"), "failure", noContact)
	if code == 0 || !out.Replayed || out.Result != nil || len(gaps) < 2 {
		t.Fatal("failed receipt replay lost outcome", out, gaps)
	}
	code, out, _ = collectionCLI(t, root, collectionPlan("queue_depth"), "auto", noContact, "--program", program)
	if code != 0 || !out.Replayed {
		t.Fatal("removed registry prevented replay", out)
	}
	db, err := store.OpenReadOnly(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	run, err := ledger.New(db, nil).Load(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range run.Entries {
		for _, secret := range []string{"private-script-body", "fixture-read-credential", "structuredContent", "audit", "arguments", "input_tokens"} {
			if bytes.Contains(entry.Payload, []byte(secret)) {
				t.Fatalf("raw value persisted: %s", secret)
			}
		}
	}
}

type collectionTransport func(*http.Request) (*http.Response, error)

func (f collectionTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCollectConcurrentLockAndCancellation(t *testing.T) {
	for _, cancelCollection := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelCollection), func(t *testing.T) {
			root, measure, _ := collectionFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			service := collect.Service{Sources: sources.Service{HTTPClient: &http.Client{Transport: collectionTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/api/v1/query" {
					close(entered)
					select {
					case <-release:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				}
				return http.DefaultTransport.RoundTrip(req)
			})}}}
			opts, err := parseCollect([]string{"--repo", root, "--plan", "-", "--run", "run", "--id", "entry"})
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				out *collectionOutput
				err error
			}
			completed := make(chan outcome, 1)
			go func() {
				out, _, err := executeCollect(ctx, opts, strings.NewReader(collectionPlan("queue_depth")), service)
				completed <- outcome{out, err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("collection never reached blocked source")
			}
			out, _, err := executeCollect(t.Context(), opts, strings.NewReader(collectionPlan("queue_depth")), collect.Service{Runtime: forbiddenCollectRuntime{t}})
			if err == nil || !strings.Contains(err.Error(), "busy") || out.Result != nil {
				t.Fatal("concurrent collection not refused", out, err)
			}
			_, calls := measure.snapshot("mcp-metrics", "queue_depth")
			if calls != 1 {
				t.Fatal("second collection contacted source", calls)
			}
			if cancelCollection {
				cancel()
			} else {
				close(release)
			}
			var first outcome
			select {
			case first = <-completed:
			case <-time.After(8 * time.Second):
				t.Fatal("collection failed to finish")
			}
			if !first.out.Committed || first.out.Result == nil || (first.err != nil) != cancelCollection {
				t.Fatal("receipt not committed after collection", first.out, first.err)
			}
			if _, err := os.Lstat(filepath.Join(root, ".sre-agent", "collect.lock")); !os.IsNotExist(err) {
				t.Fatal("owned lock leaked", err)
			}
			code, replay, _ := collectionCLI(t, root, collectionPlan("queue_depth"), "entry", collect.Service{Runtime: forbiddenCollectRuntime{t}})
			if !replay.Replayed || (code != 0) != cancelCollection {
				t.Fatal("retry did not replay original outcome", replay)
			}
			_, calls = measure.snapshot("mcp-metrics", "queue_depth")
			if calls != 1 {
				t.Fatal("retry contacted source", calls)
			}
		})
	}
}

func TestCollectRejectsStaleAndSymlinkLocks(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "file"} {
		t.Run(kind, func(t *testing.T) {
			root := ledgerRepo(t)
			ledgerData(t, root, "", "run", "start", "lock fixture", "--id", "run")
			lock := filepath.Join(root, ".sre-agent", "collect.lock")
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(lock, 0700)
			case "symlink":
				err = os.Symlink(target, lock)
			case "file":
				err = os.WriteFile(lock, []byte("keep"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			code, out, _ := collectionCLI(t, root, collectPlanJSON, "entry", collect.Service{Runtime: forbiddenCollectRuntime{t}})
			if code == 0 || out.Result != nil || out.Committed {
				t.Fatal("occupied lock collected", out)
			}
			if _, err := os.Lstat(lock); err != nil {
				t.Fatal("removed unowned lock", err)
			}
			raw, err := os.ReadFile(target)
			if err != nil || string(raw) != "keep" {
				t.Fatal("lock symlink target changed", err)
			}
		})
	}
}

func TestCollectPersistenceFailuresRetainLiveResult(t *testing.T) {
	for _, failure := range []string{"clock", "database", "conflict"} {
		t.Run(failure, func(t *testing.T) {
			root, measure, _ := collectionFixture(t)
			service := collect.Service{}
			switch failure {
			case "clock":
				var calls int
				service.Sources.Now = func() time.Time {
					calls++
					if calls > 2 {
						return time.Time{}
					}
					return time.Now()
				}
			case "database", "conflict":
				service.Sources.HTTPClient = &http.Client{Transport: collectionTransport(func(req *http.Request) (*http.Response, error) {
					response, err := http.DefaultTransport.RoundTrip(req)
					if req.URL.Path == "/api/v1/query" {
						db, openErr := store.Open(t.Context(), root)
						if openErr != nil {
							t.Error(openErr)
							return response, err
						}
						defer db.Close()
						if failure == "database" {
							_, openErr = db.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_collection BEFORE INSERT ON ledger_entries BEGIN SELECT RAISE(FAIL, 'private-database-error'); END`)
						} else {
							_, openErr = ledger.New(db, nil).Add(t.Context(), ledger.AddRequest{RunID: "run", ID: "entry", Kind: ledger.KindEvidence, Payload: json.RawMessage(`{"fact":"other writer","source":"operator"}`)})
						}
						if openErr != nil {
							t.Error(openErr)
						}
					}
					return response, err
				})}
			}
			code, out, gaps := collectionCLI(t, root, collectionPlan("queue_depth"), "entry", service)
			if code == 0 || out.Committed || out.Result == nil || !out.Result.RequiredKeysSatisfied || !strings.Contains(strings.Join(gaps, " "), "Do not repeat collection automatically") {
				t.Fatal("persistence failure hid live outcome", out, gaps)
			}
			if _, err := os.Stat(filepath.Join(root, ".sre-agent", "collect.lock")); !os.IsNotExist(err) {
				t.Fatal("write failure leaked lock", err)
			}
			for _, id := range []string{"mcp-metrics", "utcp-metrics"} {
				_, calls := measure.snapshot(id, "queue_depth")
				if calls != 1 {
					t.Fatal("write failure repeated source", id, calls)
				}
			}
		})
	}
}

type collectionShortWriter struct{}

func (collectionShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestCollectOutputFailuresPreserveCommittedIdentity(t *testing.T) {
	root, _, _ := collectionFixture(t)
	for i, writer := range []io.Writer{ledgerBrokenWriter{}, collectionShortWriter{}} {
		var stderr bytes.Buffer
		id := fmt.Sprintf("output-%d", i)
		code := runCollect([]string{"--repo", root, "--plan", "-", "--run", "run", "--id", id}, strings.NewReader(collectionPlan("queue_depth")), writer, &stderr, nil)
		if code != 1 || !strings.Contains(stderr.String(), "committed for run run entry "+id) {
			t.Fatal("output failure lost committed identity", code, &stderr)
		}
		code, out, _ := collectionCLI(t, root, collectionPlan("queue_depth"), id, collect.Service{Runtime: forbiddenCollectRuntime{t}})
		if code != 0 || !out.Replayed {
			t.Fatal("output failure lost durable receipt", out)
		}
	}
}

func TestCollectActualFormattedOutputBudget(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			// A bounded synthetic Result isolates formatting from transport budgets.
			result := &collect.Result{Evidence: []collect.Evidence{{Data: json.RawMessage(`"` + strings.Repeat("x", 2<<20) + `"`)}}}
			out := &collectionOutput{Result: result, Committed: true, RunID: "run", EntryID: "entry", Seq: 3, Receipt: &collectionReceipt{Kind: "collection_receipt_v1"}}
			var stdout, stderr bytes.Buffer
			if code := presentCollection(format, out, []string{}, nil, &stdout, &stderr); code != 1 || stdout.Len() > 2<<20 || out.Result != nil || out.Receipt == nil || out.Begin != "" || out.Finish != "" || !strings.Contains(stdout.String(), "raw data omitted") {
				t.Fatal("formatter exceeded budget or hid receipt", code, stdout.Len(), &stderr)
			}
			if format == "text" && !strings.Contains(stdout.String(), "\n  ") {
				t.Fatal("text not indented")
			}
		})
	}
	// Compact data fits, but indentation alone crosses the output budget.
	result := &collect.Result{Audit: make([]collect.AuditEvent, 16000)}
	compact, _ := json.Marshal(result)
	indented, _ := json.MarshalIndent(result, "", "  ")
	if len(compact) >= 2<<20 || len(indented) <= 2<<20 {
		t.Fatal("invalid indentation fixture", len(compact), len(indented))
	}
	var out, stderr bytes.Buffer
	output := &collectionOutput{Result: result, Committed: true, RunID: "run", EntryID: "entry", Seq: 1}
	if code := presentCollection("text", output, []string{}, nil, &out, &stderr); code != 1 || output.Result != nil || out.Len() > 2<<20 {
		t.Fatal("text formatter budget ignored indentation", code, out.Len())
	}
}

func TestCollectRefusesMalformedAndNonReceiptEntries(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "receipt fixture", "--id", "run")
	db, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, entry := range []struct {
		id      string
		kind    ledger.Kind
		payload string
	}{
		{"ordinary", ledger.KindEvidence, `{"fact":"ordinary evidence","source":"collection"}`},
		{"malformed", ledger.KindEvidence, `{"fact":"{\"kind\":\"collection_receipt_v1\"}","source":"collection"}`},
		{"different", ledger.KindMetadata, `{"impact":"partial"}`},
	} {
		if _, err := ledger.New(db, nil).Add(t.Context(), ledger.AddRequest{RunID: "run", ID: entry.id, Kind: entry.kind, Payload: json.RawMessage(entry.payload)}); err != nil {
			t.Fatal(err)
		}
		code, out, _ := collectionCLI(t, root, collectPlanJSON, entry.id, collect.Service{Runtime: forbiddenCollectRuntime{t}, Sources: sources.Service{LookupEnv: func(string) (string, bool) { t.Error("unexpected credential lookup"); return "", false }}})
		if code == 0 || out.Result != nil || out.Committed || out.Replayed {
			t.Fatal("malformed entry authorized collection", out)
		}
	}
}

// This double exercises the host interface; it does not execute JavaScript or verify a sandbox.
type collectionHostRuntime struct{ probes, opens int }

func (r *collectionHostRuntime) Probe(context.Context) error { r.probes++; return nil }
func (r *collectionHostRuntime) Open(context.Context) (collect.Session, error) {
	r.opens++
	return collectionHostSession{}, nil
}

type collectionHostSession struct{}

func (collectionHostSession) Close(context.Context) error { return nil }
func (collectionHostSession) Run(ctx context.Context, in collect.Invocation, helper collect.Helper) (collect.Execution, error) {
	execution := collect.Execution{Values: append([]collect.Value{}, in.Values...), Audit: []collect.AuditEvent{{At: time.Now().UTC(), Permission: "host-test-only", Value: "private-audit-value"}}}
	for _, key := range in.Keys {
		if _, err := helper(key.Key); err != nil {
			return execution, err
		}
		execution.Values = append(execution.Values, collect.Value{Key: key.Key, SourceID: key.SourceID, Data: json.RawMessage(`{"private_data":"raw-program-data"}`)})
	}
	return execution, nil
}
func TestCollectHostInterfaceReceiptExcludesRawProgramDataAndAudit(t *testing.T) {
	root, _, _ := collectionFixture(t)
	program := filepath.Join(root, "program.js")
	if err := os.WriteFile(program, []byte("private-program-source"), 0600); err != nil {
		t.Fatal(err)
	}
	runtime := &collectionHostRuntime{}
	code, out, _ := collectionCLI(t, root, collectionPlan("queue_depth"), "code", collect.Service{Runtime: runtime}, "--mode", "code", "--program", program, "--approve-code")
	if code != 0 || out.Result == nil || out.Result.SelectedMode != collect.ModeCode || out.Receipt.CodeSHA256 == "" || len(out.Result.Audit) == 0 || runtime.probes != 1 || runtime.opens != 1 {
		t.Fatal("host interface collection failed", out)
	}
	db, err := store.OpenReadOnly(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	run, err := ledger.New(db, nil).Load(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"private-program-source", "private-audit-value", "raw-program-data", "fixture-read-credential", "arguments", "audit"} {
		if bytes.Contains(run.Entries[0].Payload, []byte(value)) {
			t.Fatal("private collection detail persisted", value)
		}
	}
	code, out, _ = collectionCLI(t, root, collectionPlan("queue_depth"), "code", collect.Service{Runtime: forbiddenCollectRuntime{t}}, "--mode", "code", "--program", program, "--approve-code")
	if code != 0 || !out.Replayed || out.Receipt.CodeSHA256 == "" {
		t.Fatal("code receipt failed replay", out)
	}
	code, out, _ = collectionCLI(t, root, collectionPlan("queue_depth"), "no-consent", collect.Service{Runtime: forbiddenCollectRuntime{t}}, "--mode", "code", "--program", program)
	if code == 0 || !out.Committed || out.Receipt.Reason != "code consent missing" {
		t.Fatal("manual code consent failure lost reason", out)
	}
}

func TestCollectMissingRunAndSymlinkStoreHaveNoSideEffects(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint(symlink), func(t *testing.T) {
			root := ledgerRepo(t)
			target := root
			if symlink {
				target = ledgerRepo(t)
			}
			ledgerData(t, target, "", "run", "start", "existing run", "--id", "existing")
			if symlink {
				if err := os.Symlink(filepath.Join(target, ".sre-agent"), filepath.Join(root, ".sre-agent")); err != nil {
					t.Fatal(err)
				}
			}
			code, out, _ := collectionCLI(t, root, collectPlanJSON, "entry", collect.Service{Runtime: forbiddenCollectRuntime{t}})
			if code == 0 || out.Committed || out.Result != nil {
				t.Fatal("invalid store/run collected", out)
			}
			if _, err := os.Lstat(filepath.Join(target, ".sre-agent", "collect.lock")); !os.IsNotExist(err) {
				t.Fatal("preflight acquired lock", err)
			}
			db, err := store.OpenReadOnly(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			run, err := ledger.New(db, nil).Load(t.Context(), "existing")
			if err != nil || len(run.Entries) != 0 {
				t.Fatal("preflight changed existing run", run, err)
			}
		})
	}
}

func TestCollectStrictPlanLimitsBeforeState(t *testing.T) {
	root := ledgerRepo(t)
	opts, err := parseCollect([]string{"--repo", root, "--plan", "-", "--run", "run", "--id", "entry"})
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"calls", "sources", "required", "duplicate", "unknown", "utf8"} {
		plan := collect.Plan{Operation: "join", Calls: []collect.Call{}, Required: []string{"key0"}}
		count := 1
		if scenario == "calls" {
			count = 33
		}
		if scenario == "sources" {
			count = 9
		}
		for i := 0; i < count; i++ {
			source := "metrics"
			if scenario == "sources" {
				source = fmt.Sprintf("source%d", i)
			}
			plan.Calls = append(plan.Calls, collect.Call{Key: fmt.Sprintf("key%d", i), SourceID: source, Tool: "read", Arguments: json.RawMessage(`{}`)})
		}
		if scenario == "required" {
			plan.Required = []string{"unknown"}
		}
		raw, _ := json.Marshal(plan)
		if scenario == "duplicate" {
			raw = []byte(strings.Replace(string(raw), `"operation":"join"`, `"operation":"join","operation":"filter"`, 1))
		}
		if scenario == "unknown" {
			raw = []byte(strings.Replace(string(raw), `"arguments":{}`, `"arguments":{},"extra":true`, 1))
		}
		if scenario == "utf8" {
			raw = []byte(strings.Replace(string(raw), "join", string([]byte{0xff}), 1))
		}
		out, _, err := executeCollect(t.Context(), opts, bytes.NewReader(raw), collect.Service{Runtime: forbiddenCollectRuntime{t}})
		if err == nil || out.Result != nil || out.Committed {
			t.Fatal("invalid plan reached collection", scenario, out)
		}
	}
	for _, path := range []string{filepath.Join(root, "missing.js"), filepath.Join(root, "link.js")} {
		if strings.HasSuffix(path, "link.js") {
			target := filepath.Join(root, "program.js")
			if err := os.WriteFile(target, []byte("provided"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}
		opts.program = path
		if _, _, err := executeCollect(t.Context(), opts, strings.NewReader(collectPlanJSON), collect.Service{}); err == nil || !strings.Contains(err.Error(), "program") {
			t.Fatal("nonregular program accepted", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".sre-agent")); !os.IsNotExist(err) {
		t.Fatal("invalid inputs created state", err)
	}
}
