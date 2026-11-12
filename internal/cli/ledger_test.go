package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/store"
)

func ledgerRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	t.Setenv("SRE_RUN_ID", "")
	return root
}
func ledgerCLI(t *testing.T, root, input string, args ...string) (int, map[string]any, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	args = append(args, "--repo", root)
	code := RunWithInput(args, strings.NewReader(input), &out, &stderr)
	if out.Len() > 8192 {
		t.Fatalf("output exceeded budget: %d", out.Len())
	}
	var envelope map[string]any
	decoder := json.NewDecoder(&out)
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, out.String())
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("multiple envelopes: %v", err)
	}
	return code, envelope, stderr.String()
}
func ledgerData(t *testing.T, root, input string, args ...string) map[string]any {
	t.Helper()
	code, envelope, stderr := ledgerCLI(t, root, input, args...)
	if code != 0 || envelope["ok"] != true {
		t.Fatalf("command %v: %d %+v %s", args, code, envelope, stderr)
	}
	return envelope["data"].(map[string]any)
}
func TestLedgerCommandsSelectionAndRetry(t *testing.T) {
	root := ledgerRepo(t)
	start := ledgerData(t, root, "", "--id", "one", "run", "start", "first problem", "--session-id", "harness/session α")
	if start["run_id"] != "one" || start["committed"] != true {
		t.Fatalf("start: %+v", start)
	}
	ledgerData(t, root, "", "run", "start", "second problem", "--id", "two")
	t.Setenv("SRE_RUN_ID", "one")
	payload := `{"fact":"latency rose","source":"metrics"}`
	first := ledgerData(t, root, payload, "ledger", "add", "evidence", "--id", "e1", "--file", "-")
	retry := ledgerData(t, root, payload, "--file=-", "--id=e1", "ledger", "add", "evidence")
	if first["seq"] != retry["seq"] || first["entry_id"] != "e1" {
		t.Fatalf("retry: %+v %+v", first, retry)
	}
	if data := ledgerData(t, root, "", "run", "resume"); data["run_id"] != "one" {
		t.Fatal(data)
	}
	if data := ledgerData(t, root, "", "run", "resume", "two"); data["run_id"] != "two" {
		t.Fatal(data)
	}
	t.Setenv("SRE_RUN_ID", "")
	if data := ledgerData(t, root, "", "run", "resume"); data["run_id"] != "two" {
		t.Fatal(data)
	}
	file := filepath.Join(t.TempDir(), "entry.json")
	if err := os.WriteFile(file, []byte(`{"phase":3}`), 0600); err != nil {
		t.Fatal(err)
	}
	ledgerData(t, root, "", "ledger", "add", "metadata", "--run", "one", "--id", "m1", "--file", file)
	for _, args := range [][]string{{"run", "start", "text run", "--id", "text"}, {"ledger", "add", "evidence", "--run", "text", "--id", "e1", "--file", "-"}, {"run", "resume", "text"}, {"run", "show", "text"}} {
		var out, stderr bytes.Buffer
		code := RunWithInput(append(args, "--format", "text", "--repo", root), strings.NewReader(payload), &out, &stderr)
		if code != 0 || !strings.Contains(out.String(), "INVESTIGATION LEDGER -") || !strings.Contains(out.String(), "BEGIN EXTERNAL DATA") {
			t.Fatalf("text %v: %d %s %s", args, code, &out, &stderr)
		}
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if data := ledgerData(t, nested, "", "run", "show", "one"); data["through_seq"] != float64(3) {
		t.Fatal(data)
	}
	if _, err := os.Stat(filepath.Join(nested, ".sre-agent")); !os.IsNotExist(err) {
		t.Fatalf("nested database created: %v", err)
	}
}
func TestLedgerErrorsDoNotAppend(t *testing.T) {
	root := ledgerRepo(t)
	code, env, _ := ledgerCLI(t, root, "", "run", "resume")
	if code == 0 || !strings.Contains(fmt.Sprint(env["errors"]), "no runs recorded") {
		t.Fatal(env)
	}
	ledgerData(t, root, "", "run", "start", "problem", "--id", "one")
	cases := []struct {
		args []string
		raw  string
	}{
		{[]string{"ledger", "add", "evidence", "--id", "e", "--file", "-"}, `{"fact":"f","source":"s"}`},
		{[]string{"ledger", "add", "unknown", "--run", "one", "--id", "e", "--file", "-"}, `{}`},
		{[]string{"ledger", "add", "evidence", "--run", "one", "--file", "-"}, `{"fact":"f","source":"s"}`},
	}
	for _, raw := range []string{`{`, `null`, `[]`, `{"fact":"f","source":"s","extra":1}`, `{"fact":"f","source":"s"} {}`, `{"fact":"` + strings.Repeat("x", 65536) + `","source":"s"}`} {
		cases = append(cases, struct {
			args []string
			raw  string
		}{[]string{"ledger", "add", "evidence", "--run", "one", "--id", "e", "--file", "-"}, raw})
	}
	for _, args := range [][]string{{"version", "--run", "one"}, {"run", "show", "one", "--context", "prod"}, {"run", "resume", "--offset", "-1"}, {"run", "start", "p", "--offset", "2"}, {"run", "show"}, {"run", "show", "one", "--file", "-"}} {
		cases = append(cases, struct {
			args []string
			raw  string
		}{args, ""})
	}
	for _, c := range cases {
		code, env, _ := ledgerCLI(t, root, c.raw, c.args...)
		if code == 0 || env["ok"] != false {
			t.Fatalf("accepted %v: %+v", c.args, env)
		}
	}
	data := ledgerData(t, root, "", "run", "show", "one")
	if data["through_seq"] != float64(0) {
		t.Fatalf("errors mutated ledger: %+v", data)
	}
}
func TestResumeFreshProcessNeedsNoConversationState(t *testing.T) {
	if os.Getenv("SRE_LEDGER_CLI_CHILD") == "1" {
		os.Exit(RunWithInput([]string{"run", "resume", "one", "--repo", os.Getenv("SRE_LEDGER_CLI_REPO")}, strings.NewReader(""), os.Stdout, os.Stderr))
	}
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "durable problem", "--id", "one")
	ledgerData(t, root, `{"fact":"durable fact","source":"source"}`, "ledger", "add", "evidence", "--run", "one", "--id", "e", "--file", "-")
	cmd := exec.Command(os.Args[0], "-test.run=^TestResumeFreshProcessNeedsNoConversationState$")
	cmd.Env = append(os.Environ(), "SRE_LEDGER_CLI_CHILD=1", "SRE_LEDGER_CLI_REPO="+root)
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("durable fact")) {
		t.Fatalf("fresh process: %v %s", err, out)
	}
}
func TestLedgerLargeResponseBoundedAndPaged(t *testing.T) {
	root := ledgerRepo(t)
	s, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	service := ledger.New(s, nil)
	if _, err := service.Start(t.Context(), ledger.StartRequest{ID: "large", Problem: strings.Repeat("界<&", 10000)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		raw, _ := json.Marshal(ledger.Evidence{Fact: strings.Repeat("界<&", 1000), Source: "metrics"})
		if _, err := service.Add(t.Context(), ledger.AddRequest{RunID: "large", ID: fmt.Sprintf("e%d", i), Kind: ledger.KindEvidence, Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	offset := 0
	for pages := 0; pages < 40; pages++ {
		data := ledgerData(t, root, "", "run", "show", "large", "--offset", fmt.Sprint(offset))
		view := data["view"].(map[string]any)
		for _, id := range view["entry_ids"].([]any) {
			seen[id.(string)] = true
		}
		if view["next_offset"] == nil {
			break
		}
		next := int(view["next_offset"].(float64))
		if next <= offset {
			t.Fatal("pagination did not advance")
		}
		offset = next
	}
	if len(seen) != 30 {
		t.Fatalf("only %d entries retrieved", len(seen))
	}
}

type ledgerBrokenWriter struct{}

func (ledgerBrokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestLedgerCommittedWriteSurvivesPresentationAndOutputFailure(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "problem", "--id", "one")
	payload := `{"fact":"committed fact","source":"metrics"}`
	opts := ledgerOptions{repo: root, runID: "one", id: "e1", kind: ledger.KindEvidence, file: "-"}
	for _, format := range []string{"json", "text"} {
		var out, stderr bytes.Buffer
		fail := func(context.Context, *ledger.Service, string, ledger.RenderOptions) (ledger.View, error) {
			return ledger.View{}, io.ErrUnexpectedEOF
		}
		code := runLedgerWithView("ledger add", opts, format, strings.NewReader(payload), &out, &stderr, fail)
		if code != 0 || !strings.Contains(out.String(), "committed") || strings.Contains(out.String(), "unexpected EOF") {
			t.Fatalf("lost acknowledgement: %d %s %s", code, &out, &stderr)
		}
		if format == "json" {
			var envelope map[string]any
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			data := envelope["data"].(map[string]any)
			if envelope["ok"] != true || data["committed"] != true || data["entry_id"] != "e1" || data["seq"] != float64(1) || len(envelope["gaps"].([]any)) != 1 {
				t.Fatal(envelope)
			}
		}
	}
	var stderr bytes.Buffer
	opts.id = "e2"
	if code := runLedger("ledger add", opts, "json", strings.NewReader(payload), ledgerBrokenWriter{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "committed for run one entry e2 seq 2") {
		t.Fatalf("output failure: %d %s", code, &stderr)
	}
	data := ledgerData(t, root, payload, "ledger", "add", "evidence", "--run", "one", "--id", "e2", "--file", "-")
	if data["seq"] != float64(2) || data["through_seq"] != float64(2) {
		t.Fatalf("retry appended: %+v", data)
	}
	var out bytes.Buffer
	fail := func(context.Context, *ledger.Service, string, ledger.RenderOptions) (ledger.View, error) {
		return ledger.View{}, io.ErrUnexpectedEOF
	}
	if code := runLedgerWithView("run start", ledgerOptions{repo: root, problem: "generated"}, "json", nil, &out, &stderr, fail); code != 0 {
		t.Fatalf("start: %d %s", code, &out)
	}
	var envelope map[string]any
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	runID := envelope["data"].(map[string]any)["run_id"].(string)
	if !strings.HasPrefix(runID, "run-") || len(runID) != 36 {
		t.Fatal(runID)
	}
	ledgerData(t, root, "", "run", "show", runID)
}
func TestLedgerUnwrappedResponseContainsOnlyIdentity(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "private problem", "--id", "one")
	data := ledgerData(t, root, `{"fact":"untrusted narrative","source":"source"}`, "ledger", "add", "evidence", "--run", "one", "--id", "e", "--file", "-")
	view := data["view"].(map[string]any)
	if !strings.Contains(view["text"].(string), "untrusted narrative") {
		t.Fatal(view)
	}
	delete(view, "text")
	encoded, _ := json.Marshal(data)
	if bytes.Contains(encoded, []byte("untrusted narrative")) || bytes.Contains(encoded, []byte("private problem")) {
		t.Fatalf("unwrapped text: %s", encoded)
	}
}

func TestLedgerBoundedCLIShortSiblingsAndCollectionDiagnostic(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "problem", "--id", "one")
	payload, _ := json.Marshal(ledger.Evidence{Fact: strings.Repeat("x", 10000), Source: "distinct-short-source"})
	ledgerData(t, root, string(payload), "ledger", "add", "evidence", "--run", "one", "--id", "e1", "--file", "-")
	for _, command := range []string{"resume", "show"} {
		for _, format := range []string{"json", "text"} {
			var out, stderr bytes.Buffer
			code := RunWithInput([]string{"run", command, "one", "--repo", root, "--format", format}, nil, &out, &stderr)
			if code != 0 || out.Len() > 8192 || !strings.Contains(out.String(), "distinct-short-source") {
				t.Fatalf("%s %s lost required source or exceeded budget: code=%d bytes=%d stderr=%s", command, format, code, out.Len(), &stderr)
			}
			if !strings.Contains(out.String(), "entries.e1.payload.fact") {
				t.Fatalf("%s %s lost scalar truncation path", command, format)
			}
		}
	}
	links := make([]string, 2000)
	for i := range links {
		links[i] = "short-link"
	}
	payload, _ = json.Marshal(ledger.Evidence{Fact: "short-fact", Source: "short-source", Links: links})
	ledgerData(t, root, string(payload), "ledger", "add", "evidence", "--run", "one", "--id", "huge", "--file", "-")
	ledgerData(t, root, `{"fact":"next-fact","source":"next-source"}`, "ledger", "add", "evidence", "--run", "one", "--id", "next", "--file", "-")
	for _, command := range []string{"resume", "show"} {
		data := ledgerData(t, root, "", "run", command, "one", "--offset", "1")
		view := data["view"].(map[string]any)
		if view["next_offset"] != float64(2) || view["omitted_entries"] != float64(2) {
			t.Fatalf("wrong diagnostic paging: %+v", view)
		}
		text := view["text"].(string)
		for _, want := range []string{"identity only", "Omitted payloads on this page: 1", "entries.huge.payload", "cannot recover this oversized content"} {
			if !strings.Contains(text, want) {
				t.Fatalf("diagnostic lacks %q", want)
			}
		}
		next := ledgerData(t, root, "", "run", command, "one", "--offset", "2")["view"].(map[string]any)
		if !strings.Contains(next["text"].(string), "next-source") {
			t.Fatal("next entry lost")
		}
	}
}
