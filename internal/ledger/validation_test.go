package ledger

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validationEntries(t *testing.T) []Entry {
	t.Helper()
	var entries []Entry
	appendValidationEntry(t, &entries, "expected", KindExpectedBehavior, ExpectedBehavior{Criteria: []Criterion{
		{ID: "ready", Description: "pods ready", Observation: "readiness"},
		{ID: "errors", Description: "errors absent", Observation: "error count"},
	}})
	appendValidationEntry(t, &entries, "option", KindOption, Option{Summary: "restore image", Steps: []string{}, Pros: []string{}, Cons: []string{}})
	appendValidationEntry(t, &entries, "approval", KindApproval, Approval{OptionID: "option", Decision: "approved", Statement: "apply", At: ledgerTime, Target: "prod"})
	appendValidationEntry(t, &entries, "action", KindAction, Action{At: ledgerTime, Effect: "remediation", Status: "applied", OptionID: "option", ApprovalID: "approval", Result: "fixed"})
	for _, id := range []string{"ready", "errors"} {
		appendValidationEntry(t, &entries, "v-"+id, KindValidation, validationResult(id))
	}
	return entries
}

func validationResult(id string) Validation {
	return Validation{ExpectedEntryID: "expected", CriterionID: id, ActionID: "action", Verdict: "pass", Observed: "healthy", Source: "exact query", EvidenceIDs: []string{}, At: ledgerTime}
}

func appendValidationEntry(t *testing.T, entries *[]Entry, id string, kind Kind, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	*entries = append(*entries, Entry{ID: id, RunID: "run", Seq: int64(len(*entries) + 1), Kind: kind, RecordedAt: ledgerTime, Payload: raw})
}

func replaceValidationPayload(t *testing.T, entry *Entry, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	entry.Payload = raw
}

func validationRun(t *testing.T, entries []Entry) Run {
	t.Helper()
	run, err := Reduce("run", "still investigating", ledgerTime, entries)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func checkValidation(t *testing.T, run Run, verdict string, capture bool) ValidationSummary {
	t.Helper()
	before, _ := json.Marshal(run)
	summary := EvaluateValidation(run)
	if summary.Verdict != verdict || summary.EligibleForCapture != capture {
		t.Fatalf("verdict=%s capture=%v, want %s %v: %+v", summary.Verdict, summary.EligibleForCapture, verdict, capture, summary)
	}
	validated, err := ValidateCapture(run)
	if capture {
		if err != nil || !reflect.DeepEqual(validated.Run, run) || !reflect.DeepEqual(validated.Validation, summary) {
			t.Fatalf("capture changed snapshot: %+v %v", validated, err)
		}
	} else if !errors.Is(err, ErrNotValidated) || len(summary.Reasons) == 0 || !strings.Contains(err.Error(), summary.Reasons[0]) {
		t.Fatalf("capture rejection lacks reasons: %v %+v", err, summary)
	}
	after, _ := json.Marshal(run)
	if !bytes.Equal(before, after) {
		t.Fatal("evaluator mutated input")
	}
	return summary
}

func TestValidationVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name, result, status, want string
		count                      int
		noApproval                 bool
		capture                    bool
	}{
		{name: "no criteria", count: 0, want: "unresolved"},
		{name: "no action", count: 3, want: "unresolved"},
		{name: "no results", count: 4, want: "applied_but_unverified"},
		{name: "one of two criteria", count: 5, want: "applied_but_unverified"},
		{name: "pending", count: 6, result: "pending", want: "applied_but_unverified"},
		{name: "unknown", count: 6, result: "unknown", want: "applied_but_unverified"},
		{name: "explicit fail", count: 6, result: "fail", want: "failed"},
		{name: "all pass", count: 6, want: "resolved", capture: true},
		{name: "no approval", count: 6, noApproval: true, want: "unresolved"},
		{name: "failed action", count: 6, status: "failed", want: "unresolved"},
		{name: "unverified action", count: 6, status: "unverified", want: "unresolved"},
		{name: "rolled back action", count: 6, status: "rolled_back", want: "unresolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := validationEntries(t)[:tc.count]
			if tc.result != "" {
				v := validationResult("errors")
				v.Verdict = tc.result
				replaceValidationPayload(t, &entries[5], v)
			}
			if tc.noApproval || tc.status != "" {
				a := decoded[Action](t, entries[3])
				if tc.noApproval {
					a.ApprovalID = ""
				}
				if tc.status != "" {
					a.Status = tc.status
				}
				replaceValidationPayload(t, &entries[3], a)
			}
			appendValidationEntry(t, &entries, "phase", KindMetadata, map[string]any{"phase": 6, "impact": "fixed and healthy"})
			summary := checkValidation(t, validationRun(t, entries), tc.want, tc.capture)
			if tc.count >= 4 && summary.ActionID != "action" {
				t.Fatal("selected action identity missing")
			}
			if tc.count >= 1 && (summary.ExpectedEntryID != "expected" || len(summary.Criteria) != 2) {
				t.Fatal("criteria identity missing")
			}
		})
	}
}

func TestCriteriaReplacementAndLaterActionInvalidatePasses(t *testing.T) {
	for _, kind := range []Kind{KindExpectedBehavior, KindAction} {
		t.Run(string(kind), func(t *testing.T) {
			entries := validationEntries(t)
			var payload any = decoded[ExpectedBehavior](t, entries[0])
			if kind == KindAction {
				payload = decoded[Action](t, entries[3])
			}
			appendValidationEntry(t, &entries, "replacement", kind, payload)
			summary := checkValidation(t, validationRun(t, entries), "applied_but_unverified", false)
			for _, c := range summary.Criteria {
				if c.ValidationID != "" {
					t.Fatal("stale result counted")
				}
			}
			for _, id := range []string{"ready", "errors"} {
				v := validationResult(id)
				if kind == KindAction {
					v.ActionID = "replacement"
				} else {
					v.ExpectedEntryID = "replacement"
				}
				appendValidationEntry(t, &entries, "fresh-"+id, KindValidation, v)
			}
			checkValidation(t, validationRun(t, entries), "resolved", true)
		})
	}
	entries := validationEntries(t)
	appendValidationEntry(t, &entries, "observe", KindAction, Action{At: ledgerTime, Effect: "observation", Status: "failed"})
	checkValidation(t, validationRun(t, entries), "resolved", true)
	appendValidationEntry(t, &entries, "failed", KindAction, Action{At: ledgerTime, Effect: "remediation", Status: "failed"})
	checkValidation(t, validationRun(t, entries), "unresolved", false)
}

func TestLatestFailedValidationOverridesPass(t *testing.T) {
	entries := validationEntries(t)
	v := validationResult("ready")
	v.Verdict = "fail"
	appendValidationEntry(t, &entries, "failure", KindValidation, v)
	summary := checkValidation(t, validationRun(t, entries), "failed", false)
	if summary.Criteria[0].ValidationID != "failure" || summary.Criteria[0].Verdict != "fail" {
		t.Fatal("latest result missing")
	}
	v.Verdict = "pending"
	appendValidationEntry(t, &entries, "retry", KindValidation, v)
	checkValidation(t, validationRun(t, entries), "applied_but_unverified", false)
}

func TestLatestInvalidValidationDoesNotFallBackToPass(t *testing.T) {
	entries := validationEntries(t)
	v := validationResult("ready")
	v.Source = ""
	appendValidationEntry(t, &entries, "invalid-latest", KindValidation, v)
	summary := checkValidation(t, validationRun(t, entries), "applied_but_unverified", false)
	if summary.Criteria[0].ValidationID != "invalid-latest" || summary.Criteria[0].Verdict != "unknown" {
		t.Fatal("invalid latest assessment fell back to an older pass")
	}
}

func TestNewApprovalRequiresNewActionAndResults(t *testing.T) {
	entries := validationEntries(t)
	p := decoded[Approval](t, entries[2])
	p.Decision = "revoked"
	appendValidationEntry(t, &entries, "revocation", KindApproval, p)
	p.Decision = "approved"
	appendValidationEntry(t, &entries, "new-approval", KindApproval, p)
	checkValidation(t, validationRun(t, entries), "unresolved", false)
	a := decoded[Action](t, entries[3])
	a.ApprovalID = "new-approval"
	appendValidationEntry(t, &entries, "new-action", KindAction, a)
	checkValidation(t, validationRun(t, entries), "applied_but_unverified", false)
	for _, id := range []string{"ready", "errors"} {
		v := validationResult(id)
		v.ActionID = "new-action"
		appendValidationEntry(t, &entries, "fresh-"+id, KindValidation, v)
	}
	checkValidation(t, validationRun(t, entries), "resolved", true)
}

func TestFailedCriterionWithoutApprovalRemainsFailed(t *testing.T) {
	entries := validationEntries(t)
	a := decoded[Action](t, entries[3])
	a.ApprovalID = ""
	replaceValidationPayload(t, &entries[3], a)
	v := validationResult("ready")
	v.Verdict = "fail"
	replaceValidationPayload(t, &entries[4], v)
	checkValidation(t, validationRun(t, entries), "failed", false)
}

func TestApprovalRevocationBlocksCapture(t *testing.T) {
	for _, tc := range []string{"revoked", "rejected", "mismatched option", "future approval", "later revocation", "unrelated revocation", "rollback", "safety violation"} {
		t.Run(tc, func(t *testing.T) {
			entries := validationEntries(t)
			want, capture := "unresolved", false
			p := decoded[Approval](t, entries[2])
			switch tc {
			case "revoked", "rejected":
				p.Decision = tc
				replaceValidationPayload(t, &entries[2], p)
			case "mismatched option":
				p.OptionID = "absent"
				replaceValidationPayload(t, &entries[2], p)
			case "future approval":
				p.At = ledgerTime.Add(time.Second)
				replaceValidationPayload(t, &entries[2], p)
			case "later revocation":
				p.Decision = "revoked"
				appendValidationEntry(t, &entries, "revoke", KindApproval, p)
			case "unrelated revocation":
				appendValidationEntry(t, &entries, "other-option", KindOption, decoded[Option](t, entries[1]))
				p.OptionID, p.Decision = "other-option", "revoked"
				appendValidationEntry(t, &entries, "revoke", KindApproval, p)
				want, capture = "resolved", true
			case "rollback":
				a := decoded[Action](t, entries[3])
				a.Effect = "rollback"
				replaceValidationPayload(t, &entries[3], a)
				want, capture = "resolved", true
			case "safety violation":
				appendValidationEntry(t, &entries, "violation", KindSafetyViolation, SafetyViolation{Wave: 1, Investigators: []string{"logs"}, Changes: []SafetyChange{{Object: "deployment", Change: "changed"}}})
				want, capture = "resolved", true
			}
			checkValidation(t, validationRun(t, entries), want, capture)
		})
	}
}

func TestOptionalObservationWindow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seconds   int64
		duration  time.Duration
		endpoints bool
		want      string
	}{
		{"omitted", 0, 0, false, "resolved"},
		{"missing endpoints", 900, 0, false, "applied_but_unverified"},
		{"short", 900, 899 * time.Second, true, "applied_but_unverified"},
		{"fraction short", 900, 900*time.Second - time.Nanosecond, true, "applied_but_unverified"},
		{"exact", 900, 900 * time.Second, true, "resolved"},
		{"huge", math.MaxInt64, 900 * time.Second, true, "applied_but_unverified"},
		{"duration overflow", 1 << 34, time.Second, true, "applied_but_unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := validationEntries(t)
			expected := decoded[ExpectedBehavior](t, entries[0])
			if tc.seconds != 0 {
				expected.Criteria[0].WindowSeconds = &tc.seconds
			}
			replaceValidationPayload(t, &entries[0], expected)
			v := validationResult("ready")
			v.At = ledgerTime.Add(tc.duration)
			entries[4].RecordedAt = v.At
			if tc.endpoints {
				start, end := ledgerTime, v.At
				v.WindowStart, v.WindowEnd = &start, &end
			}
			replaceValidationPayload(t, &entries[4], v)
			checkValidation(t, validationRun(t, entries), tc.want, tc.want == "resolved")
		})
	}
}

func TestObservationWindowBeyondDurationRange(t *testing.T) {
	entries := validationEntries(t)
	x := decoded[ExpectedBehavior](t, entries[0])
	seconds := int64(10_000_000_000)
	x.Criteria[0].WindowSeconds = &seconds
	replaceValidationPayload(t, &entries[0], x)
	end := time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	v := validationResult("ready")
	v.At, v.WindowStart, v.WindowEnd = end, &ledgerTime, &end
	entries[4].RecordedAt = end
	replaceValidationPayload(t, &entries[4], v)
	checkValidation(t, validationRun(t, entries), "resolved", true)
}

func TestCaptureSnapshotDoesNotAliasInput(t *testing.T) {
	run := validationRun(t, validationEntries(t))
	validated, err := ValidateCapture(run)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(validated)
	run.Entries[0].Payload[0] = ' '
	run.State.Expected.Payload[0] = ' '
	after, _ := json.Marshal(validated)
	if !bytes.Equal(before, after) {
		t.Fatal("caller mutation changed validated snapshot")
	}
}

func TestValidationFreshnessAndStructure(t *testing.T) {
	for _, tc := range []string{"empty observed", "empty source", "before action", "before criteria", "future pass", "reversed window", "window before action", "window after observation", "one endpoint", "wrong evidence", "early sequence"} {
		t.Run(tc, func(t *testing.T) {
			entries := validationEntries(t)
			v := validationResult("ready")
			past, future := ledgerTime.Add(-time.Second), ledgerTime.Add(time.Second)
			switch tc {
			case "empty observed":
				v.Observed = " "
			case "empty source":
				v.Source = "\t"
			case "before action":
				a := decoded[Action](t, entries[3])
				a.At = future
				replaceValidationPayload(t, &entries[3], a)
			case "before criteria":
				entries[0].RecordedAt = future
			case "future pass":
				v.At = future
			case "reversed window":
				v.WindowStart, v.WindowEnd = &future, &past
			case "wrong evidence":
				v.EvidenceIDs = []string{"option"}
			case "window before action", "window after observation", "one endpoint":
				x := decoded[ExpectedBehavior](t, entries[0])
				seconds := int64(1)
				x.Criteria[0].WindowSeconds = &seconds
				replaceValidationPayload(t, &entries[0], x)
				if tc == "window before action" {
					v.WindowStart, v.WindowEnd = &past, &ledgerTime
				}
				if tc == "window after observation" {
					v.WindowStart, v.WindowEnd = &ledgerTime, &future
				}
				if tc == "one endpoint" {
					v.WindowStart = &ledgerTime
				}
			}
			replaceValidationPayload(t, &entries[4], v)
			if tc == "early sequence" {
				entries[3], entries[4] = entries[4], entries[3]
				entries[3].Seq, entries[4].Seq = 4, 5
			}
			checkValidation(t, validationRun(t, entries), "applied_but_unverified", false)
		})
	}
}

func seedValidationService(t *testing.T, s *Service) {
	t.Helper()
	startLedger(t, s, "run", "")
	for _, e := range validationEntries(t) {
		if _, err := s.Add(t.Context(), AddRequest{RunID: "run", ID: e.ID, Kind: e.Kind, Payload: e.Payload}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCrossRunCriterionReferencesRejected(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	seedValidationService(t, s)
	startLedger(t, s, "other", "")
	addLedger(t, s, "other", "other-expected", KindExpectedBehavior, ExpectedBehavior{Criteria: []Criterion{{ID: "foreign", Description: "other", Observation: "query"}}})
	addLedger(t, s, "other", "other-evidence", KindEvidence, Evidence{Fact: "healthy", Source: "query"})
	for _, tc := range []string{"criterion", "expected", "evidence"} {
		v := validationResult("ready")
		switch tc {
		case "criterion":
			v.CriterionID = "foreign"
		case "expected":
			v.ExpectedEntryID = "other-expected"
		case "evidence":
			v.EvidenceIDs = []string{"other-evidence"}
		}
		raw, _ := json.Marshal(v)
		if _, err := s.Add(t.Context(), AddRequest{RunID: "run", ID: "invalid", Kind: KindValidation, Payload: raw}); !errors.Is(err, ErrInvalidReference) {
			t.Fatalf("%s: %v", tc, err)
		}
	}
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries WHERE run_id='run'") != 6 || ledgerInt(t, db.DB(), "SELECT next_seq FROM runs WHERE id='run'") != 7 {
		t.Fatal("invalid references changed history")
	}
}

func TestValidationServiceRejectsInvalidObservations(t *testing.T) {
	for _, tc := range []string{"observed", "source", "stale", "future", "window", "fail source"} {
		t.Run(tc, func(t *testing.T) {
			s, db := openLedger(t, t.TempDir())
			seedValidationService(t, s)
			v := validationResult("ready")
			switch tc {
			case "observed":
				v.Observed = ""
			case "source":
				v.Source = " "
			case "stale":
				v.At = ledgerTime.Add(-time.Nanosecond)
			case "future":
				v.At = ledgerTime.Add(time.Nanosecond)
			case "window":
				start, end := ledgerTime, ledgerTime.Add(-time.Nanosecond)
				v.WindowStart, v.WindowEnd = &start, &end
			case "fail source":
				v.Verdict, v.Source = "fail", ""
			}
			raw, _ := json.Marshal(v)
			if _, err := s.Add(t.Context(), AddRequest{RunID: "run", ID: "invalid", Kind: KindValidation, Payload: raw}); err == nil {
				t.Fatal("invalid observation accepted")
			}
			if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries") != 6 || ledgerInt(t, db.DB(), "SELECT next_seq FROM runs") != 7 {
				t.Fatal("invalid observation changed history")
			}
		})
	}
}

func TestValidationServiceWindowAndIncompleteResults(t *testing.T) {
	for _, tc := range []struct {
		name, verdict string
		duration      time.Duration
		window        bool
		wantErr       bool
	}{
		{"short pass", "pass", 899 * time.Second, true, true},
		{"exact pass", "pass", 900 * time.Second, true, false},
		{"missing window", "pass", 900 * time.Second, false, true},
		{"pending incomplete window", "pending", 1 * time.Second, true, false},
		{"unknown future window", "unknown", 901 * time.Second, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db := openLedger(t, t.TempDir())
			seedValidationService(t, s)
			seconds := int64(900)
			x := ExpectedBehavior{Criteria: []Criterion{{ID: "ready", Description: "stays ready", Observation: "ready count", WindowSeconds: &seconds}}}
			addLedger(t, s, "run", "window-criteria", KindExpectedBehavior, x)
			now := ledgerTime.Add(900 * time.Second)
			s.now = func() time.Time { return now }
			v := validationResult("ready")
			v.ExpectedEntryID, v.Verdict, v.At = "window-criteria", tc.verdict, now
			if tc.verdict != "pass" {
				v.Source, v.Observed = "", ""
			}
			if tc.window {
				end := ledgerTime.Add(tc.duration)
				v.WindowStart, v.WindowEnd = &ledgerTime, &end
			}
			raw, _ := json.Marshal(v)
			req := AddRequest{RunID: "run", ID: "window-result", Kind: KindValidation, Payload: raw}
			entry, err := s.Add(t.Context(), req)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidValidation) {
					t.Fatalf("invalid window: %v", err)
				}
				if ledgerInt(t, db.DB(), "SELECT next_seq FROM runs") != 8 || ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_entries") != 7 {
					t.Fatal("invalid window persisted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Hour)
			replay, err := s.Add(t.Context(), req)
			if err != nil || !reflect.DeepEqual(entry, replay) {
				t.Fatalf("validation replay changed: %v", err)
			}
			run, err := s.Load(t.Context(), "run")
			if err != nil {
				t.Fatal(err)
			}
			want := "applied_but_unverified"
			if tc.verdict == "pass" {
				want = "resolved"
			}
			checkValidation(t, run, want, tc.verdict == "pass")
		})
	}
}

func TestValidationServiceRejectsDamagedReadAndReplay(t *testing.T) {
	for _, tc := range []string{"criterion", "future"} {
		t.Run(tc, func(t *testing.T) {
			s, db := openLedger(t, t.TempDir())
			seedValidationService(t, s)
			v := validationResult("ready")
			if tc == "criterion" {
				v.CriterionID = "missing"
			} else {
				v.At = ledgerTime.Add(time.Second)
			}
			raw, _ := json.Marshal(v)
			if _, err := db.DB().ExecContext(t.Context(), "UPDATE ledger_entries SET payload_json=? WHERE id='v-ready'", string(raw)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(t.Context(), "run"); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("damaged read: %v", err)
			}
			if _, err := s.Add(t.Context(), AddRequest{RunID: "run", ID: "v-ready", Kind: KindValidation, Payload: raw}); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("damaged replay: %v", err)
			}
		})
	}
}

func TestCaptureReadsOneSnapshot(t *testing.T) {
	root := t.TempDir()
	s, db := openLedger(t, root)
	seedValidationService(t, s)
	if _, err := db.DB().ExecContext(t.Context(), "CREATE TABLE capture_fixture(run_id TEXT, through_seq INTEGER, snapshot TEXT)"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCaptureProcess$")
	cmd.Env = append(os.Environ(), "SRE_CAPTURE_ROOT="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "READY" {
		t.Fatalf("child not ready: %s", scanner.Text())
	}
	tx, err := db.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "UPDATE runs SET next_seq=next_seq WHERE id='run'"); err != nil {
		t.Fatal(err)
	}
	run, err := LoadRun(t.Context(), tx, "run")
	if err != nil {
		t.Fatal(err)
	}
	validated, err := ValidateCapture(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(stdin, "append"); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() || scanner.Text() != "BLOCKED" {
		t.Fatalf("append not serialized: %s %s", scanner.Text(), stderr.String())
	}
	again, err := LoadRun(t.Context(), tx, "run")
	if err != nil || !reflect.DeepEqual(again, run) {
		t.Fatal("capture snapshot changed while locked")
	}
	raw, _ := json.Marshal(validated)
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO capture_fixture VALUES (?,?,?)", run.ID, run.ThroughSeq, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(stdin, "retry"); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() || scanner.Text() != "APPENDED" {
		t.Fatalf("append after capture: %s %s", scanner.Text(), stderr.String())
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v %s", err, stderr.String())
	}
	var captured string
	if err := db.DB().QueryRowContext(t.Context(), "SELECT snapshot FROM capture_fixture WHERE run_id='run' AND through_seq=6").Scan(&captured); err != nil {
		t.Fatal(err)
	}
	var saved ValidatedRun
	if err := json.Unmarshal([]byte(captured), &saved); err != nil || !reflect.DeepEqual(saved, validated) {
		t.Fatal("capture identity or contents changed")
	}
	latest, err := s.Load(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if latest.ThroughSeq != 7 {
		t.Fatal("later append lost")
	}
	checkValidation(t, latest, "applied_but_unverified", false)
}

func TestCaptureProcess(t *testing.T) {
	root := os.Getenv("SRE_CAPTURE_ROOT")
	if root == "" {
		return
	}
	s, db := openLedger(t, root)
	if _, err := db.DB().ExecContext(t.Context(), "PRAGMA busy_timeout=25"); err != nil {
		t.Fatal(err)
	}
	fmt.Println("READY")
	input := bufio.NewScanner(os.Stdin)
	if !input.Scan() {
		t.Fatal("no append signal")
	}
	raw, _ := json.Marshal(ExpectedBehavior{Criteria: []Criterion{{ID: "fresh", Description: "fresh check", Observation: "query"}}})
	req := AddRequest{RunID: "run", ID: "replacement", Kind: KindExpectedBehavior, Payload: raw}
	if _, err := s.Add(t.Context(), req); err == nil {
		t.Fatal("append succeeded during capture write transaction")
	}
	fmt.Println("BLOCKED")
	if !input.Scan() {
		t.Fatal("no retry signal")
	}
	if _, err := s.Add(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	fmt.Println("APPENDED")
}
