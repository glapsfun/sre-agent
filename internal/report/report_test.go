package report

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
)

var reportTime = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func addEntry(t *testing.T, entries *[]ledger.Entry, id string, kind ledger.Kind, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	*entries = append(*entries, ledger.Entry{ID: id, RunID: "run", Seq: int64(len(*entries) + 1), Kind: kind, RecordedAt: reportTime.Add(time.Hour), Payload: raw})
}

func runFrom(t *testing.T, entries []ledger.Entry) ledger.Run {
	t.Helper()
	run, err := ledger.Reduce("run", "Checkout latency", reportTime.Add(-time.Hour), entries)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func validatedEntries(t *testing.T) []ledger.Entry {
	t.Helper()
	var entries []ledger.Entry
	impact, detection := "Checkout requests were slow", "Latency alert"
	links := []string{"https://example.test/incident", "https://example.test/dashboard"}
	addEntry(t, &entries, "metadata", ledger.KindMetadata, ledger.MetadataUpdate{Impact: &impact, StartedAt: &reportTime, Detection: &detection, Links: &links})
	addEntry(t, &entries, "latency", ledger.KindEvidence, ledger.Evidence{Fact: "Queue grew after rollout", Source: "kubectl get pods -n checkout", ObservedAt: &reportTime, Links: []string{links[1]}})
	addEntry(t, &entries, "cause", ledger.KindHypothesis, ledger.Hypothesis{Statement: "Worker concurrency was reduced", Mechanism: "Fewer workers increased queue time", Confidence: "high", Supporting: []string{"latency"}, Contradicting: []string{}, Status: "confirmed"})
	addEntry(t, &entries, "rejected", ledger.KindHypothesis, ledger.Hypothesis{Statement: "Database saturation", Mechanism: "Database latency stayed normal", Confidence: "low", Supporting: []string{}, Contradicting: []string{"latency"}, DiscriminatingTest: "compare database latency", Status: "ruled_out"})
	window := int64(60)
	addEntry(t, &entries, "expected", ledger.KindExpectedBehavior, ledger.ExpectedBehavior{Criteria: []ledger.Criterion{{ID: "latency", Description: "Checkout latency recovers", Observation: "p99 latency", Threshold: "below 1s", WindowSeconds: &window, Source: "prom query checkout_latency"}}})
	addEntry(t, &entries, "option", ledger.KindOption, ledger.Option{Summary: "Restore worker concurrency", Steps: []string{"restore workers"}, Risk: "rollout", Pros: []string{}, Cons: []string{}, ExpectedImpact: "queue drains", Rollback: "restore previous worker count", HypothesisIDs: []string{"cause"}})
	addEntry(t, &entries, "approval", ledger.KindApproval, ledger.Approval{OptionID: "option", Decision: "approved", Statement: "Apply the worker change", At: reportTime, Target: "checkout"})
	actionAt := reportTime.Add(time.Minute)
	addEntry(t, &entries, "action", ledger.KindAction, ledger.Action{At: actionAt, Command: "kubectl apply -f workers.yaml", Result: "Workers restored", Effect: "remediation", Status: "applied", OptionID: "option", ApprovalID: "approval", EvidenceIDs: []string{"latency"}, Rollback: "kubectl rollout undo deploy/checkout"})
	end := reportTime.Add(2 * time.Minute)
	addEntry(t, &entries, "healthy", ledger.KindEvidence, ledger.Evidence{Fact: "p99 recovered", Source: "prom query checkout_latency", ObservedAt: &end})
	addEntry(t, &entries, "validation", ledger.KindValidation, ledger.Validation{ExpectedEntryID: "expected", CriterionID: "latency", ActionID: "action", Verdict: "pass", Observed: "0.4s", Source: "prom query checkout_latency", EvidenceIDs: []string{"healthy"}, At: reportTime.Add(time.Hour), WindowStart: &actionAt, WindowEnd: &end})
	return entries
}

func assertGolden(t *testing.T, document Document, name string) {
	t.Helper()
	want, err := os.ReadFile("testdata/" + name + ".md")
	if err != nil {
		t.Fatal(err)
	}
	if document.Markdown != string(want) {
		t.Fatalf("markdown differs from %s golden:\n%s", name, document.Markdown)
	}
	headings := []string{"Summary", "Impact and start time", "Timeline", "Root cause", "Decisive evidence", "Fix and rollback", "Validation results", "Hypotheses ruled out", "Links"}
	previous := -1
	for _, heading := range headings {
		index := strings.Index(document.Markdown, "## "+heading+"\n")
		if index <= previous {
			t.Fatalf("missing or out-of-order section %s", heading)
		}
		previous = index
	}
}

func TestValidatedReportGolden(t *testing.T) {
	run := runFrom(t, validatedEntries(t))
	before, _ := json.Marshal(run)
	document, err := Build(run)
	if err != nil {
		t.Fatal(err)
	}
	if document.Status != "resolved" || document.RunID != "run" || document.ThroughSeq != 10 || len(document.Gaps) != 0 {
		t.Fatalf("wrong document: %+v", document)
	}
	assertGolden(t, document, "validated")
	for _, command := range []string{"kubectl get pods -n checkout", "kubectl apply -f workers.yaml", "prom query checkout\\_latency", "kubectl rollout undo deploy/checkout"} {
		if !strings.Contains(document.Markdown, command) {
			t.Errorf("missing source command %q", command)
		}
	}
	after, _ := json.Marshal(run)
	if string(before) != string(after) {
		t.Fatal("Build mutated the ledger")
	}
}

func TestUnresolvedReportGolden(t *testing.T) {
	entries := validatedEntries(t)[:8]
	phase := 6
	problem := "Narrative says fixed"
	addEntry(t, &entries, "phase", ledger.KindMetadata, ledger.MetadataUpdate{Phase: &phase, Problem: &problem})
	document, err := Build(runFrom(t, entries))
	if err != nil {
		t.Fatal(err)
	}
	if document.Status != "unresolved" {
		t.Fatalf("status = %s", document.Status)
	}
	assertGolden(t, document, "unresolved")
}

func TestUnresolvedValidationStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		keep   int
		change string
		want   string
	}{
		{"no fix", 5, "", "no remediation or rollback recorded"},
		{"no criteria", 4, "", "no expected behavior recorded"},
		{"missing", 8, "", "no current validation recorded"},
		{"pending", 10, "pending", "current validation is pending"},
		{"failed", 10, "fail", "current validation is fail"},
		{"changed criteria", 10, "criteria", "no current validation recorded"},
		{"later action", 10, "action", "no current validation recorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := validatedEntries(t)[:tc.keep]
			switch tc.change {
			case "pending", "fail":
				payload, _ := ledger.DecodePayload(entries[9].Kind, entries[9].Payload)
				validation := payload.(ledger.Validation)
				validation.Verdict = tc.change
				entries[9].Payload, _ = json.Marshal(validation)
			case "criteria":
				payload, _ := ledger.DecodePayload(entries[4].Kind, entries[4].Payload)
				addEntry(t, &entries, "new-criteria", ledger.KindExpectedBehavior, payload)
			case "action":
				payload, _ := ledger.DecodePayload(entries[7].Kind, entries[7].Payload)
				addEntry(t, &entries, "new-action", ledger.KindAction, payload)
			}
			phase := 6
			addEntry(t, &entries, "phase", ledger.KindMetadata, ledger.MetadataUpdate{Phase: &phase})
			document, err := Build(runFrom(t, entries))
			if err != nil {
				t.Fatal(err)
			}
			if document.Status != "unresolved" || !strings.Contains(document.Markdown, tc.want) {
				t.Fatalf("unexpected resolution: %+v", document)
			}
		})
	}
}

func TestValidatedUndeclaredWindowAndRollbackFallback(t *testing.T) {
	entries := validatedEntries(t)
	payload, _ := ledger.DecodePayload(entries[4].Kind, entries[4].Payload)
	expected := payload.(ledger.ExpectedBehavior)
	expected.Criteria[0].WindowSeconds = nil
	entries[4].Payload, _ = json.Marshal(expected)
	payload, _ = ledger.DecodePayload(entries[9].Kind, entries[9].Payload)
	validation := payload.(ledger.Validation)
	validation.WindowStart, validation.WindowEnd = nil, nil
	entries[9].Payload, _ = json.Marshal(validation)
	payload, _ = ledger.DecodePayload(entries[7].Kind, entries[7].Payload)
	action := payload.(ledger.Action)
	action.Rollback = ""
	entries[7].Payload, _ = json.Marshal(action)
	document, err := Build(runFrom(t, entries))
	if err != nil {
		t.Fatal(err)
	}
	if document.Status != "resolved" || !strings.Contains(document.Markdown, "Declared window: not recorded") || !strings.Contains(document.Markdown, "Rollback: restore previous worker count") {
		t.Fatalf("invented window or lost rollback: %+v", document)
	}
}

func TestUnknownFactsAndStableOrder(t *testing.T) {
	var entries []ledger.Entry
	links := []string{"https://example.test/one", "https://example.test/one"}
	addEntry(t, &entries, "metadata", ledger.KindMetadata, ledger.MetadataUpdate{Links: &links})
	later := reportTime.Add(time.Minute)
	addEntry(t, &entries, "late", ledger.KindEvidence, ledger.Evidence{Fact: "Later observation", Source: "late query", ObservedAt: &later})
	addEntry(t, &entries, "z-first", ledger.KindEvidence, ledger.Evidence{Fact: "First equal time", Source: "first query", ObservedAt: &reportTime})
	addEntry(t, &entries, "a-second", ledger.KindEvidence, ledger.Evidence{Fact: "Second equal time", Source: "second query", ObservedAt: &reportTime})
	addEntry(t, &entries, "undated", ledger.KindEvidence, ledger.Evidence{Fact: "Undated observation", Source: "undated query", Links: []string{links[0], "https://example.test/two"}})
	run := runFrom(t, entries)
	document, err := Build(run)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := Build(run)
		if err != nil || !reflect.DeepEqual(document, again) {
			t.Fatalf("unstable output: %v", err)
		}
	}
	for _, value := range []string{"Start time: not recorded", "Impact: not recorded", "Undated evidence:", "Root cause not confirmed.", "No evidence cited by confirmed hypotheses, the selected action or current validation."} {
		if !strings.Contains(document.Markdown, value) {
			t.Errorf("missing %q", value)
		}
	}
	previous := -1
	for _, value := range []string{"First equal time", "Second equal time", "Later observation", "Undated observation"} {
		index := strings.Index(document.Markdown, value)
		if index <= previous {
			t.Errorf("timeline out of order: %s", value)
		}
		previous = index
	}
	if strings.Contains(document.Markdown, "09:00:00") || strings.Count(document.Markdown, "example.test/one") != 1 || strings.Count(document.Markdown, "example.test/two") != 1 {
		t.Fatalf("fabricated start time or duplicate links:\n%s", document.Markdown)
	}
}

func TestUnknownHistoricalHypothesesAndOmittedDetail(t *testing.T) {
	entries := validatedEntries(t)
	payload, _ := ledger.DecodePayload(entries[1].Kind, entries[1].Payload)
	evidence := payload.(ledger.Evidence)
	evidence.Fact = "\nConcise evidence\nraw detail must be omitted"
	entries[1].Payload, _ = json.Marshal(evidence)
	payload, _ = ledger.DecodePayload(entries[7].Kind, entries[7].Payload)
	action := payload.(ledger.Action)
	action.Result = "Concise result\r\nraw result must be omitted"
	entries[7].Payload, _ = json.Marshal(action)
	addEntry(t, &entries, "reopened", ledger.KindHypothesis, ledger.Hypothesis{Statement: "Database saturation reconsidered", Mechanism: "New investigation", Confidence: "low", Supporting: []string{}, Contradicting: []string{}, Status: "open", Supersedes: "rejected"})
	addEntry(t, &entries, "second-cause", ledger.KindHypothesis, ledger.Hypothesis{Statement: "Queue capacity also reduced", Mechanism: "Fewer queued requests", Confidence: "high", Supporting: []string{"latency"}, Contradicting: []string{}, Status: "confirmed"})
	document, err := Build(runFrom(t, entries))
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Gaps) != 2 || strings.Contains(document.Markdown, "raw detail") || strings.Contains(document.Markdown, "raw result") {
		t.Fatalf("omitted detail mishandled: %+v", document)
	}
	for _, value := range []string{"Multiple confirmed hypotheses recorded.", "Database latency stayed normal", "Contradicting evidence: latency", "Concise evidence", "Concise result"} {
		if !strings.Contains(document.Markdown, value) {
			t.Errorf("missing %q", value)
		}
	}
}

type runReader func(context.Context, string) (ledger.Run, error)

func (reader runReader) Load(ctx context.Context, id string) (ledger.Run, error) {
	return reader(ctx, id)
}

func TestGenerateSelectedRunAndErrors(t *testing.T) {
	var _ RunReader = (*ledger.Service)(nil)
	run := runFrom(t, validatedEntries(t))
	document, err := Generate(context.Background(), runReader(func(ctx context.Context, id string) (ledger.Run, error) {
		if id != "run" {
			t.Fatalf("loaded wrong run: %s", id)
		}
		return run, nil
	}), "run")
	if err != nil || document.Status != "resolved" {
		t.Fatalf("generate: %+v %v", document, err)
	}
	_, err = Generate(context.Background(), runReader(func(context.Context, string) (ledger.Run, error) { return ledger.Run{}, ledger.ErrNotFound }), "absent")
	if !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("missing error: %v", err)
	}
}

func TestUnknownSnapshotAndCanonicalState(t *testing.T) {
	run := runFrom(t, validatedEntries(t))
	run.State.Impact = "uncommitted impact"
	run.State.Hypotheses = nil
	document, err := Build(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(document.Markdown, "uncommitted impact") || !strings.Contains(document.Markdown, "Worker concurrency was reduced") {
		t.Fatalf("trusted derived state over committed history:\n%s", document.Markdown)
	}
	run.ThroughSeq++
	document, err = Build(run)
	if err != nil || document.Status != "unresolved" || !strings.Contains(document.Markdown, "invalid ledger snapshot") {
		t.Fatalf("invalid snapshot accepted or not reported: %+v %v", document, err)
	}
	run.Entries[0].Payload = json.RawMessage(`{"unknown":true}`)
	document, err = Build(run)
	if err != nil || document.Status != "unresolved" || len(document.Gaps) != 1 || !strings.Contains(document.Markdown, "ledger details unavailable") {
		t.Fatalf("malformed snapshot lacks useful unresolved report: %+v %v", document, err)
	}
}

func TestGenerateReadBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := runReader(func(context.Context, string) (ledger.Run, error) {
		t.Fatal("unexpected read")
		return ledger.Run{}, nil
	})
	if _, err := Generate(ctx, reader, "run"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if _, err := Generate(context.Background(), reader, ""); err == nil {
		t.Fatal("empty run accepted")
	}
	if _, err := Generate(context.Background(), runReader(func(context.Context, string) (ledger.Run, error) {
		return ledger.Run{ID: "different"}, nil
	}), "run"); err == nil {
		t.Fatal("different run accepted")
	}
}

func TestUnknownMultilineStructuredDetail(t *testing.T) {
	entries := validatedEntries(t)
	payload, _ := ledger.DecodePayload(entries[1].Kind, entries[1].Payload)
	evidence := payload.(ledger.Evidence)
	evidence.Fact = "{\n  \"detail\": \"raw structured evidence\",\n  \"more\": \"additional evidence output\"\n}"
	entries[1].Payload, _ = json.Marshal(evidence)
	payload, _ = ledger.DecodePayload(entries[7].Kind, entries[7].Payload)
	action := payload.(ledger.Action)
	action.Result = "[\n  \"raw structured result\",\n  \"additional action output\"\n]"
	entries[7].Payload, _ = json.Marshal(action)
	document, err := Build(runFrom(t, entries))
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"raw structured", "additional evidence output", "additional action output"} {
		if strings.Contains(document.Markdown, unwanted) {
			t.Fatalf("retained collapsed multiline body %q:\n%s", unwanted, document.Markdown)
		}
	}
	if len(document.Gaps) != 2 || !strings.Contains(strings.Join(document.Gaps, "\n"), "redaction changed line structure; excerpt omitted") {
		t.Fatalf("missing structured omission accounting: %+v", document.Gaps)
	}
	for _, required := range []string{"latency:", "action:", "kubectl get pods -n checkout", "kubectl apply -f workers.yaml"} {
		if !strings.Contains(document.Markdown, required) {
			t.Errorf("lost source identity %q", required)
		}
	}
}
