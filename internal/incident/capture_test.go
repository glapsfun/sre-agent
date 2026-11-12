package incident

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/store"
)

var incidentTime = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

type fixtureEntry struct {
	id      string
	kind    ledger.Kind
	payload any
}

func captureFixture() []fixtureEntry {
	window := int64(60)
	start, end := incidentTime, incidentTime.Add(time.Minute)
	impact, problem := "checkout unavailable", "checkout fails"
	links := []string{"https://example.test/incident"}
	return []fixtureEntry{
		{"metadata", ledger.KindMetadata, ledger.MetadataUpdate{Problem: &problem, Impact: &impact, StartedAt: &start, Links: &links,
			Environment: &ledger.Environment{Context: "production", Namespace: "shop", Workload: "checkout", GitOpsManager: "flux", Cloud: "cloud",
				SourceRepos: []ledger.Repository{{Path: "src", Remote: "https://example.test/src"}}, GitOpsRepos: []ledger.Repository{{Path: "deploy", Remote: "https://example.test/deploy"}},
				CloudCoordinates: map[string]string{"region": "west"}, Notes: []string{"environment note"}}}},
		{"evidence", ledger.KindEvidence, ledger.Evidence{Fact: "pods crash", Source: "kubectl get pods", ObservedAt: &start, Links: links}},
		{"hypothesis", ledger.KindHypothesis, ledger.Hypothesis{Statement: "bad image", Mechanism: "startup regression", Confidence: "high", Supporting: []string{"evidence"}, Contradicting: []string{}, DiscriminatingTest: "compare images", Status: "confirmed"}},
		{"expected", ledger.KindExpectedBehavior, ledger.ExpectedBehavior{Criteria: []ledger.Criterion{
			{ID: "ready", Description: "pods ready", Observation: "ready count", Threshold: "all", WindowSeconds: &window, Source: "kubectl get pods"},
			{ID: "errors", Description: "errors absent", Observation: "error count", Threshold: "zero", Source: "sum(errors)"},
		}, Reason: "restore service"}},
		{"option", ledger.KindOption, ledger.Option{Summary: "restore image", Steps: []string{"deploy previous image"}, Risk: "brief restart", Pros: []string{"known image"}, Cons: []string{"old version"}, ExpectedImpact: "restore checkout", Rollback: "restore current image", HypothesisIDs: []string{"hypothesis"}}},
		{"approval", ledger.KindApproval, ledger.Approval{OptionID: "option", Decision: "approved", Statement: "apply previous image", At: start, Target: "production"}},
		{"action", ledger.KindAction, ledger.Action{At: start, Command: "kubectl set image", Result: "image restored", Effect: "remediation", Status: "applied", OptionID: "option", ApprovalID: "approval", EvidenceIDs: []string{"evidence"}, Rollback: "restore current image"}},
		{"v-ready", ledger.KindValidation, ledger.Validation{ExpectedEntryID: "expected", CriterionID: "ready", ActionID: "action", Verdict: "pass", Observed: "all ready", Source: "kubectl get pods", EvidenceIDs: []string{"evidence"}, At: end, WindowStart: &start, WindowEnd: &end, Reason: "stable"}},
		{"v-errors", ledger.KindValidation, ledger.Validation{ExpectedEntryID: "expected", CriterionID: "errors", ActionID: "action", Verdict: "pass", Observed: "zero", Source: "sum(errors)", EvidenceIDs: []string{"evidence"}, At: end, Reason: "healthy"}},
	}
}

func openCaptureStore(t *testing.T, root string) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedCapture(t *testing.T, s *store.Store, entries []fixtureEntry) *ledger.Service {
	t.Helper()
	l := ledger.New(s, func() time.Time { return incidentTime.Add(time.Minute) })
	if _, err := l.Start(t.Context(), ledger.StartRequest{ID: "run", Problem: "checkout fails"}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		appendCaptureEntry(t, l, entry)
	}
	return l
}

func appendCaptureEntry(t *testing.T, l *ledger.Service, entry fixtureEntry) ledger.Entry {
	t.Helper()
	raw, err := json.Marshal(entry.payload)
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Add(t.Context(), ledger.AddRequest{RunID: "run", ID: entry.id, Kind: entry.kind, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func captureOptions() CaptureOptions {
	return CaptureOptions{RunID: "run", Service: "checkout", Symptom: "crashloop", Fingerprint: "startup failure"}
}

func captureNow() time.Time { return incidentTime.Add(time.Hour) }

func incidentRow(t *testing.T, s *store.Store) Incident {
	t.Helper()
	var got Incident
	var at, raw string
	if err := s.DB().QueryRowContext(t.Context(), "SELECT run_id,through_seq,captured_at,service,symptom,fingerprint,record_json FROM incidents WHERE run_id='run'").Scan(
		&got.RunID, &got.ThroughSeq, &at, &got.Service, &got.Symptom, &got.Fingerprint, &raw); err != nil {
		t.Fatal(err)
	}
	var err error
	got.CapturedAt, err = time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &got.Record); err != nil {
		t.Fatal(err)
	}
	return got
}

func incidentCount(t *testing.T, s *store.Store) int {
	t.Helper()
	var count int
	if err := s.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM incidents").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCaptureRequiresCurrentValidatedFix(t *testing.T) {
	for _, name := range []string{"no criteria", "no action", "no results", "missing result", "pending", "failed", "unknown", "changed criteria", "later remediation", "missing approval", "revoked approval", "rolled back", "valid", "applied rollback", "optional windows"} {
		t.Run(name, func(t *testing.T) {
			entries := captureFixture()
			valid := name == "valid" || name == "applied rollback" || name == "optional windows"
			switch name {
			case "no criteria":
				entries = append(entries[:3], entries[4:7]...)
			case "no action":
				entries = entries[:6]
			case "no results":
				entries = entries[:7]
			case "missing result":
				entries = entries[:8]
			case "pending", "failed", "unknown":
				v := entries[8].payload.(ledger.Validation)
				v.Verdict = name
				if name == "failed" {
					v.Verdict = "fail"
				}
				entries[8].payload = v
			case "changed criteria":
				entries = append(entries, fixtureEntry{"new-expected", ledger.KindExpectedBehavior, entries[3].payload})
			case "later remediation":
				entries = append(entries, fixtureEntry{"new-action", ledger.KindAction, entries[6].payload})
			case "missing approval", "rolled back", "applied rollback":
				a := entries[6].payload.(ledger.Action)
				switch name {
				case "missing approval":
					a.ApprovalID = ""
				case "rolled back":
					a.Status = "rolled_back"
				case "applied rollback":
					a.Effect = "rollback"
				}
				entries[6].payload = a
			case "revoked approval":
				a := entries[5].payload.(ledger.Approval)
				a.Decision = "revoked"
				entries = append(entries, fixtureEntry{"revoked", ledger.KindApproval, a})
			case "optional windows":
				e := entries[3].payload.(ledger.ExpectedBehavior)
				e.Criteria[0].WindowSeconds = nil
				entries[3].payload = e
				v := entries[7].payload.(ledger.Validation)
				v.WindowStart, v.WindowEnd = nil, nil
				entries[7].payload = v
			}
			s := openCaptureStore(t, t.TempDir())
			seedCapture(t, s, entries)
			got, err := New(s, captureNow).Capture(t.Context(), captureOptions())
			if !valid {
				if !errors.Is(err, ledger.ErrNotValidated) {
					t.Fatalf("want shared validation rejection, got %v", err)
				}
				if incidentCount(t, s) != 0 {
					t.Fatal("invalid run persisted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.RunID != "run" || got.ThroughSeq != 9 || !got.CapturedAt.Equal(captureNow()) || !reflect.DeepEqual(incidentRow(t, s), got) {
				t.Fatalf("wrong committed snapshot: %+v", got)
			}
			if got.Record.Problem != "checkout fails" || got.Record.Impact != "checkout unavailable" || got.Record.StartedAt == nil || !got.Record.StartedAt.Equal(incidentTime) || got.Record.Environment.Namespace != "shop" {
				t.Fatal("metadata projection incomplete")
			}
			if !reflect.DeepEqual(got.Record.Expected, entries[3].payload) || !reflect.DeepEqual(got.Record.Fix, entries[6].payload) || got.Record.Option == nil || !reflect.DeepEqual(*got.Record.Option, entries[4].payload) {
				t.Fatal("criteria, fix or rollback changed")
			}
			if len(got.Record.Evidence) != 1 || got.Record.Evidence[0].ID != "evidence" || got.Record.Evidence[0].Evidence.Source != "kubectl get pods" || len(got.Record.Hypotheses) != 1 || got.Record.Hypotheses[0].Hypothesis.Statement != "bad image" {
				t.Fatal("source-cited evidence or hypotheses missing")
			}
			if got.Record.Validation.Verdict != "resolved" || !got.Record.Validation.EligibleForCapture || got.Record.Validation.ActionID != "action" || len(got.Record.Results) != 2 || got.Record.Results[0].ID != "v-ready" || !reflect.DeepEqual(got.Record.Results[0].Validation, entries[7].payload) || !reflect.DeepEqual(got.Record.Results[1].Validation, entries[8].payload) {
				t.Fatal("current validation observations, sources or windows missing")
			}
		})
	}
}

func TestCaptureRaceAndRetry(t *testing.T) {
	root := t.TempDir()
	s := openCaptureStore(t, root)
	seedCapture(t, s, captureFixture())
	other := openCaptureStore(t, root)
	if _, err := other.DB().ExecContext(t.Context(), "PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	svc := New(s, func() time.Time {
		close(entered)
		<-release
		return captureNow()
	})
	type outcome struct {
		incident Incident
		err      error
	}
	finished := make(chan outcome, 1)
	go func() {
		got, err := svc.Capture(t.Context(), captureOptions())
		finished <- outcome{got, err}
	}()
	select {
	case <-entered:
	case got := <-finished:
		t.Fatalf("capture exited before transaction barrier: %v", got.err)
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not reach barrier")
	}
	l := ledger.New(other, func() time.Time { return incidentTime.Add(time.Minute) })
	v := captureFixture()[8].payload.(ledger.Validation)
	v.Verdict = "fail"
	raw, _ := json.Marshal(v)
	req := ledger.AddRequest{RunID: "run", ID: "failure", Kind: ledger.KindValidation, Payload: raw}
	if _, err := l.Add(t.Context(), req); err == nil {
		t.Fatal("ledger append crossed capture's write lock")
	}
	if incidentCount(t, other) != 0 {
		t.Fatal("uncommitted capture visible to another connection")
	}
	close(release)
	first := <-finished
	if first.err != nil || first.incident.ThroughSeq != 9 || incidentCount(t, other) != 1 {
		t.Fatalf("capture did not commit one snapshot: %+v %v", first.incident, first.err)
	}
	later := captureNow().Add(time.Hour)
	svc = New(other, func() time.Time { return later })
	retry, err := svc.Capture(t.Context(), captureOptions())
	if err != nil || !reflect.DeepEqual(retry, first.incident) {
		t.Fatalf("retry changed capture time or contents: %+v %v", retry, err)
	}
	if _, err := l.Add(t.Context(), req); err != nil {
		t.Fatalf("append did not resume after capture: %v", err)
	}
	if _, err := svc.Capture(t.Context(), captureOptions()); !errors.Is(err, ledger.ErrNotValidated) || !reflect.DeepEqual(incidentRow(t, other), first.incident) {
		t.Fatalf("invalid recapture replaced validated row: %v", err)
	}
	v.Verdict, v.Observed = "pass", "healthy again"
	appendCaptureEntry(t, l, fixtureEntry{"revalidated", ledger.KindValidation, v})
	updated, err := svc.Capture(t.Context(), captureOptions())
	if err != nil || updated.ThroughSeq != 11 || !updated.CapturedAt.Equal(later) || len(updated.Record.Results) != 2 || updated.Record.Results[1].ID != "revalidated" || incidentCount(t, other) != 1 {
		t.Fatalf("valid recapture did not replace one row: %+v %v", updated, err)
	}
	opts := captureOptions()
	opts.Fingerprint = "new startup signature"
	later = later.Add(time.Hour)
	updated, err = svc.Capture(t.Context(), opts)
	if err != nil || updated.Fingerprint != opts.Fingerprint || !updated.CapturedAt.Equal(later) || incidentCount(t, other) != 1 {
		t.Fatalf("signature reconciliation failed: %+v %v", updated, err)
	}
	t.Log("capture held writer lock at sequence 9; competing ledger append failed until commit; retry retained time; invalid sequence 10 preserved row; validated sequence 11 replaced one row")
}

func TestCaptureWaitsForWriterBeforeLoading(t *testing.T) {
	root := t.TempDir()
	s := openCaptureStore(t, root)
	seedCapture(t, s, captureFixture())
	other := openCaptureStore(t, root)
	conn, err := other.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	approval := captureFixture()[5].payload.(ledger.Approval)
	approval.Decision = "revoked"
	raw, _ := json.Marshal(approval)
	if _, err := conn.ExecContext(t.Context(), "INSERT INTO ledger_entries VALUES ('run',10,'revocation','approval',?,?)", incidentTime.Format(time.RFC3339Nano), string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "UPDATE runs SET next_seq=11 WHERE id='run'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(t.Context(), "PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	called := false
	svc := New(s, func() time.Time { called = true; return captureNow() })
	if _, err := svc.Capture(t.Context(), captureOptions()); err == nil || called {
		t.Fatalf("capture read a snapshot before obtaining the write lock: %v", err)
	}
	if _, err := conn.ExecContext(t.Context(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Capture(t.Context(), captureOptions()); !errors.Is(err, ledger.ErrNotValidated) {
		t.Fatalf("capture ignored committed revocation: %v", err)
	}
	if incidentCount(t, s) != 0 {
		t.Fatal("stale capture persisted")
	}
}

// Preserve typed identities and enums while poisoning every free-text field.
func poisonText(value reflect.Value) {
	if value.Type() == reflect.TypeFor[time.Time]() {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			poisonText(value.Elem())
		}
	case reflect.Struct:
		for i := range value.NumField() {
			switch value.Type().Field(i).Name {
			case "ID", "Supporting", "Contradicting", "Supersedes", "HypothesisIDs", "OptionID", "ApprovalID", "EvidenceIDs", "ExpectedEntryID", "CriterionID", "ActionID", "Confidence", "Status", "Effect", "Decision", "Verdict":
				continue
			}
			poisonText(value.Field(i))
		}
	case reflect.Slice:
		for i := range value.Len() {
			poisonText(value.Index(i))
		}
	case reflect.Map:
		value.Set(reflect.ValueOf(map[string]string{"password": "fixture-sensitive-value", "token=fixture-sensitive-value": "token=fixture-sensitive-value", "region": "token=fixture-sensitive-value"}))
	case reflect.String:
		value.SetString("token=fixture-sensitive-value")
	}
}

func TestCaptureRedactsProjection(t *testing.T) {
	root := t.TempDir()
	s := openCaptureStore(t, root)
	entries := captureFixture()
	seedCapture(t, s, entries)
	for _, entry := range entries {
		value := reflect.New(reflect.TypeOf(entry.payload)).Elem()
		value.Set(reflect.ValueOf(entry.payload))
		poisonText(value)
		if entry.kind == ledger.KindEvidence {
			value.FieldByName("Fact").SetString("\n\nfirst token=fixture-sensitive-value\nRAW-LOG-DUMP\nmore detail")
		}
		if entry.kind == ledger.KindAction {
			value.FieldByName("Result").SetString("\nfirst token=fixture-sensitive-value\nRAW-ACTION-DUMP")
		}
		raw, err := json.Marshal(value.Interface())
		if err != nil {
			t.Fatal(err)
		}
		// Bypass ledger redaction to prove capture owns its persistence boundary.
		if _, err := s.DB().ExecContext(t.Context(), "UPDATE ledger_entries SET payload_json=? WHERE run_id='run' AND id=?", string(raw), entry.id); err != nil {
			t.Fatal(err)
		}
	}
	opts := captureOptions()
	opts.Service, opts.Fingerprint = "token=fixture-sensitive-value", "password=fixture-sensitive-value"
	got, err := New(s, captureNow).Capture(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.Record.Evidence[0].Evidence.Fact != "first token=[REDACTED]" || got.Record.Fix.Result != "first token=[REDACTED]" || len(got.Record.Gaps) != 2 {
		t.Fatalf("multiline summaries or omission gaps missing: %+v", got.Record)
	}
	if _, retained := got.Record.Environment.CloudCoordinates["token=fixture-sensitive-value"]; retained {
		t.Fatal("decoding redacted projection retained an original coordinate key")
	}
	for _, data := range []Incident{got, incidentRow(t, s)} {
		raw, _ := json.Marshal(data)
		for _, forbidden := range []string{"fixture-sensitive-value", "RAW-LOG-DUMP", "RAW-ACTION-DUMP", `"payload"`, `"payload_json"`, `"entries"`} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("projection retained %q", forbidden)
			}
		}
		if !strings.Contains(string(raw), "[REDACTED]") {
			t.Fatal("redaction fixture did not reach projection")
		}
	}
	var stored string
	if err := s.DB().QueryRowContext(t.Context(), "SELECT record_json FROM incidents WHERE run_id='run'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"fixture-sensitive-value", "RAW-LOG-DUMP", "RAW-ACTION-DUMP", `"payload"`, `"payload_json"`, `"entries"`} {
		if strings.Contains(stored, forbidden) {
			t.Fatalf("stored JSON retained %q", forbidden)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openCaptureStore(t, root)
	retry, err := New(reopened, func() time.Time { return captureNow().Add(time.Hour) }).Capture(t.Context(), opts)
	if err != nil || !reflect.DeepEqual(retry, got) || !reflect.DeepEqual(incidentRow(t, reopened), got) {
		t.Fatalf("redacted capture did not survive reopen: %v", err)
	}
}

func TestCaptureRejectsMalformedPayloadWithoutMutation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "recapture"}[existing], func(t *testing.T) {
			s := openCaptureStore(t, t.TempDir())
			seedCapture(t, s, captureFixture())
			svc := New(s, captureNow)
			var before Incident
			if existing {
				var err error
				before, err = svc.Capture(t.Context(), captureOptions())
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.DB().ExecContext(t.Context(), `UPDATE ledger_entries SET payload_json='{"fact":42,"source":"query"}' WHERE id='evidence'`); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Capture(t.Context(), captureOptions()); !errors.Is(err, ledger.ErrCorrupt) {
				t.Fatalf("malformed payload accepted: %v", err)
			}
			if existing {
				if !reflect.DeepEqual(incidentRow(t, s), before) {
					t.Fatal("malformed recapture changed prior row")
				}
			} else if incidentCount(t, s) != 0 {
				t.Fatal("malformed capture wrote partial row")
			}
		})
	}
}

func TestCaptureCancellationAndWriteFailure(t *testing.T) {
	s := openCaptureStore(t, t.TempDir())
	l := seedCapture(t, s, captureFixture())
	ctx, cancel := context.WithCancel(t.Context())
	_, err := New(s, func() time.Time { cancel(); return captureNow() }).Capture(ctx, captureOptions())
	if !errors.Is(err, context.Canceled) || incidentCount(t, s) != 0 {
		t.Fatalf("canceled capture committed or wrong error: %v", err)
	}
	svc := New(s, captureNow)
	before, err := svc.Capture(t.Context(), captureOptions())
	if err != nil {
		t.Fatalf("cancellation left an open transaction: %v", err)
	}
	appendCaptureEntry(t, l, fixtureEntry{"more", ledger.KindEvidence, ledger.Evidence{Fact: "still healthy", Source: "query"}})
	if _, err := s.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_capture BEFORE UPDATE ON incidents BEGIN SELECT RAISE(ABORT, 'token=fixture-trigger-value'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Capture(t.Context(), captureOptions()); err == nil || strings.Contains(err.Error(), "fixture-trigger-value") {
		t.Fatalf("write failure succeeded or leaked driver diagnostic: %v", err)
	}
	if !reflect.DeepEqual(incidentRow(t, s), before) {
		t.Fatal("failed upsert changed prior capture")
	}
	appendCaptureEntry(t, l, fixtureEntry{"after-failure", ledger.KindEvidence, ledger.Evidence{Fact: "healthy", Source: "query"}})
}

func TestCaptureCommitFailureRollsBack(t *testing.T) {
	s := openCaptureStore(t, t.TempDir())
	l := seedCapture(t, s, captureFixture())
	svc := New(s, captureNow)
	before, err := svc.Capture(t.Context(), captureOptions())
	if err != nil {
		t.Fatal(err)
	}
	appendCaptureEntry(t, l, fixtureEntry{"more", ledger.KindEvidence, ledger.Evidence{Fact: "healthy", Source: "query"}})
	// A deferred foreign key lets the upsert succeed but rejects COMMIT.
	if _, err := s.DB().ExecContext(t.Context(), `CREATE TABLE commit_failure (run_id TEXT REFERENCES runs(id) DEFERRABLE INITIALLY DEFERRED);
CREATE TRIGGER fail_commit AFTER UPDATE ON incidents BEGIN INSERT INTO commit_failure VALUES ('missing'); END`); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Capture(t.Context(), captureOptions())
	if err == nil || got.RunID != "" {
		t.Fatalf("failed commit returned success: %+v %v", got, err)
	}
	if !reflect.DeepEqual(incidentRow(t, s), before) {
		t.Fatal("failed commit changed previous snapshot")
	}
	var count int
	if err := s.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM commit_failure").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed commit left transaction writes: count=%d error=%v", count, err)
	}
	appendCaptureEntry(t, l, fixtureEntry{"after-commit-failure", ledger.KindEvidence, ledger.Evidence{Fact: "healthy", Source: "query"}})
}

func TestCaptureInputAndConnectionSettings(t *testing.T) {
	s := openCaptureStore(t, t.TempDir())
	seedCapture(t, s, captureFixture())
	for _, opts := range []CaptureOptions{
		{Service: "checkout", Symptom: "crashloop", Fingerprint: "failure"},
		{RunID: "run", Service: " ", Symptom: "crashloop", Fingerprint: "failure"},
		{RunID: "run", Service: "checkout", Symptom: "crashloop", Fingerprint: " "},
		{RunID: "run", Service: "checkout", Symptom: "performance", Fingerprint: "failure"},
		{RunID: "run", Service: "checkout", Symptom: "crashloop", Fingerprint: string([]byte{0xff})},
	} {
		if _, err := New(s, captureNow).Capture(t.Context(), opts); err == nil {
			t.Fatalf("invalid options accepted: %+v", opts)
		} else if opts.Symptom == "performance" {
			for _, symptom := range []string{"crashloop", "latency", "errors", "metrics", "oom", "networking", "availability"} {
				if !strings.Contains(err.Error(), symptom) {
					t.Fatalf("symptom error omits canonical choices: %v", err)
				}
			}
		}
	}
	if incidentCount(t, s) != 0 {
		t.Fatal("invalid input changed incidents")
	}
	for _, at := range []time.Time{{}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if _, err := New(s, func() time.Time { return at }).Capture(t.Context(), captureOptions()); err == nil {
			t.Fatal("invalid capture time accepted")
		}
	}
	// Expire the original connection so capture must configure its own writer.
	s.DB().SetMaxIdleConns(0)
	conn, err := s.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.DB().SetMaxIdleConns(1)
	if _, err := conn.ExecContext(t.Context(), "PRAGMA foreign_keys=OFF; PRAGMA synchronous=OFF"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(s, nil).Capture(t.Context(), captureOptions()); err != nil {
		t.Fatal(err)
	}
	for pragma, want := range map[string]int{"foreign_keys": 1, "synchronous": 2, "busy_timeout": 5000} {
		var got int
		if err := s.DB().QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s=%d, want %d: %v", pragma, got, want, err)
		}
	}
	if _, err := s.DB().ExecContext(t.Context(), "UPDATE incidents SET run_id='missing'"); err == nil {
		t.Fatal("incident run foreign key not enforced")
	}
	if _, err := s.DB().ExecContext(t.Context(), "UPDATE incidents SET record_json='invalid'"); err == nil {
		t.Fatal("invalid incident JSON accepted")
	}
}
