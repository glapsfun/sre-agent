package incident

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/store"
)

func recallOptions() RecallOptions {
	return RecallOptions{Service: "checkout", Symptom: "oom", Fingerprint: "pod checkout-abcdef1234-abcde OOMKilled exit 137 request 550e8400-e29b-41d4-a716-446655440000"}
}

func recallFixtures(t *testing.T) []Incident {
	t.Helper()
	raw, err := os.ReadFile("testdata/incidents.json")
	if err != nil {
		t.Fatal(err)
	}
	var incidents []Incident
	if err := json.Unmarshal(raw, &incidents); err != nil {
		t.Fatal(err)
	}
	return incidents
}

func matchIDs(matches []Match) []string {
	ids := make([]string, len(matches))
	for i, match := range matches {
		ids[i] = match.Incident.RunID
	}
	return ids
}

func TestCheckoutOOMRanksFirst(t *testing.T) {
	matches := Rank(recallOptions(), recallFixtures(t))
	want := []string{"checkout-oom-a", "checkout-oom-z", "checkout-oom-old", "checkout-wrong-symptom", "other-service", "unrelated"}
	if got := matchIDs(matches); !reflect.DeepEqual(got, want) {
		t.Fatalf("tier, timestamp and run order = %v, want %v", got, want)
	}
	for i, score := range []int{399, 399, 399, 299, 199, 0} {
		if matches[i].Score != score {
			t.Errorf("%s score = %d, want %d", want[i], matches[i].Score, score)
		}
	}
	if matches[0].Similarity != 1 || matches[4].Similarity != 1 {
		t.Fatalf("variable pod suffixes, IDs and numbers must normalize equally: %v", matches)
	}
}

func TestRecallDeterministicTopAndReasons(t *testing.T) {
	incidents := recallFixtures(t)
	original := append([]Incident(nil), incidents...)
	want := []string{"checkout-oom-a", "checkout-oom-z", "checkout-oom-old", "checkout-wrong-symptom", "other-service", "unrelated"}
	rng := rand.New(rand.NewSource(17))
	t.Run("order and reasons", func(t *testing.T) {
		for iteration := 0; iteration < 20; iteration++ {
			rng.Shuffle(len(incidents), func(i, j int) { incidents[i], incidents[j] = incidents[j], incidents[i] })
			before := append([]Incident(nil), incidents...)
			matches := Rank(recallOptions(), incidents)
			if got := matchIDs(matches); !reflect.DeepEqual(got, want) {
				t.Fatalf("shuffle %d order = %v, want %v", iteration, got, want)
			}
			if !reflect.DeepEqual(incidents, before) {
				t.Fatal("Rank mutated its input")
			}
			for _, match := range matches {
				reasons := strings.Join(match.Reasons, " ")
				service, symptom := "service differs", "symptom differs"
				if match.Incident.Service == "checkout" {
					service = "service matches"
				}
				if match.Incident.Symptom == "oom" {
					symptom = "symptom matches"
				}
				for _, reason := range []string{service, symptom, "normalized fingerprint similarity", "not proof"} {
					if !strings.Contains(reasons, reason) {
						t.Errorf("%s lacks %q in %v", match.Incident.RunID, reason, match.Reasons)
					}
				}
			}
		}
	})

	s := openCaptureStore(t, t.TempDir())
	seedRecall(t, s, original)
	for _, top := range []int{0, 1, 3, 6, 20} {
		opts := recallOptions()
		opts.Top = top
		got, err := New(s, nil).Recall(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		count := top
		if count == 0 {
			count = 5
		}
		if count > 6 {
			count = 6
		}
		if got.Total != 6 || !reflect.DeepEqual(matchIDs(got.Matches), want[:count]) {
			t.Errorf("top %d: total=%d matches=%v", top, got.Total, matchIDs(got.Matches))
		}
	}
}

func TestRecallRedactsStoredData(t *testing.T) {
	s := openCaptureStore(t, t.TempDir())
	seedCapture(t, s, captureFixture())
	got, err := New(s, captureNow).Capture(t.Context(), captureOptions())
	if err != nil {
		t.Fatal(err)
	}
	got.Record.Problem = "token=recall-private-problem"
	got.Record.Environment.CloudCoordinates = map[string]string{"password=recall-private-key": "token=recall-private-coordinate"}
	raw, err := json.Marshal(got.Record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(t.Context(), "UPDATE incidents SET service=?,fingerprint=?,record_json=?", "token=recall-private-service", "token=recall-private-stored OOMKilled", string(raw)); err != nil {
		t.Fatal(err)
	}
	opts := RecallOptions{Service: "token=recall-private-query", Symptom: "crashloop", Fingerprint: "token=recall-private-search OOMKilled"}
	result, err := New(s, nil).Recall(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Score != 399 || result.Matches[0].Similarity != 1 {
		t.Fatalf("query and stored values must share current redaction/masking: %v", result)
	}
	raw, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "recall-private-") {
		t.Fatalf("recall leaked stored or query credentials: %s", raw)
	}
}

func TestRecallSimilarity(t *testing.T) {
	for _, tc := range []struct {
		name, query, candidate string
		want                   float64
		score                  int
	}{
		{"set overlap", "alpha beta beta", "beta gamma", 1.0 / 3, 333},
		{"round halves up", "alpha beta", "alpha", 0.5, 350},
		{"unrelated static words", "OOMKilled", "PermissionDenied", 0, 300},
		{"empty query", " \t\n", "OOMKilled", 0, 300},
		{"empty candidate", "OOMKilled", " \n", 0, 300},
		{"both empty", "", "", 0, 300},
		{"case preserved", "OOMKilled", "oomkilled", 0, 300},
		{"redact before mask", "token=recall-private-alpha OOMKilled", "token=recall-private-beta OOMKilled", 1, 399},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := RecallOptions{Service: "checkout", Symptom: "oom", Fingerprint: tc.query}
			got := Rank(query, []Incident{{Service: "checkout", Symptom: "oom", Fingerprint: tc.candidate}})
			if len(got) != 1 || math.Abs(got[0].Similarity-tc.want) > 1e-12 || got[0].Score != tc.score {
				t.Fatalf("matches=%v, want similarity %v score %d", got, tc.want, tc.score)
			}
		})
	}
	t.Run("exact service", func(t *testing.T) {
		got := Rank(recallOptions(), []Incident{{Service: "Checkout", Symptom: "oom", Fingerprint: "unrelated"}, {Service: "checkout ", Symptom: "oom", Fingerprint: "unrelated"}})
		if len(got) != 2 || got[0].Score != 100 || got[1].Score != 100 {
			t.Fatalf("service names must compare exactly: %v", got)
		}
	})
	t.Run("similarity breaks rounded score ties", func(t *testing.T) {
		tokens := []string{"needle"}
		for i := 0; i < 200; i++ {
			tokens = append(tokens, string([]rune{'x', rune('a' + i/26), rune('a' + i%26)}))
		}
		query := RecallOptions{Service: "checkout", Symptom: "oom", Fingerprint: "needle"}
		got := Rank(query, []Incident{
			{RunID: "lower", Service: "checkout", Symptom: "oom", Fingerprint: strings.Join(tokens, " "), CapturedAt: incidentTime},
			{RunID: "higher", Service: "checkout", Symptom: "oom", Fingerprint: strings.Join(tokens[:200], " "), CapturedAt: incidentTime.Add(-time.Hour)},
		})
		if len(got) != 2 || got[0].Incident.RunID != "higher" || got[0].Score != 300 || got[1].Score != 300 {
			t.Fatalf("unrounded similarity must precede recency: %v", got)
		}
	})
}

func seedRecall(t *testing.T, s *store.Store, incidents []Incident) {
	t.Helper()
	l := seedCapture(t, s, captureFixture())
	captured, err := New(s, captureNow).Capture(t.Context(), captureOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(t.Context(), "DELETE FROM incidents"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(captured.Record)
	if err != nil {
		t.Fatal(err)
	}
	for _, incident := range incidents {
		if _, err := l.Start(t.Context(), ledger.StartRequest{ID: incident.RunID, Problem: "fixture incident"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(t.Context(), "INSERT INTO incidents(run_id,through_seq,captured_at,service,symptom,fingerprint,record_json) VALUES(?,?,?,?,?,?,?)",
			incident.RunID, incident.ThroughSeq, incident.CapturedAt.Format(time.RFC3339Nano), incident.Service, incident.Symptom, incident.Fingerprint, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecallEmptyAndErrors(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		s := openCaptureStore(t, t.TempDir())
		got, err := New(s, nil).Recall(t.Context(), recallOptions())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `{"matches":[],"total":0,"message":"no incident history"}` {
			t.Fatalf("empty history = %s", raw)
		}
	})
	for _, tc := range []struct{ name, sql string }{
		{"invalid JSON", `UPDATE incidents SET record_json='{'`},
		{"wrong JSON type", `UPDATE incidents SET record_json='{"problem":42}'`},
		{"array record", `UPDATE incidents SET record_json='[]'`},
		{"null record", `UPDATE incidents SET record_json='null'`},
		{"empty record", `UPDATE incidents SET record_json='{}'`},
		{"ineligible record", `UPDATE incidents SET record_json=json_set(record_json,'$.validation.eligible_for_capture',json('false'))`},
		{"invalid time", `UPDATE incidents SET captured_at='not-a-time'`},
		{"zero time", `UPDATE incidents SET captured_at='0001-01-01T00:00:00Z'`},
		{"non UTC time", `UPDATE incidents SET captured_at='2026-11-12T10:00:00+02:00'`},
		{"invalid sequence", `UPDATE incidents SET through_seq=0`},
		{"empty service", `UPDATE incidents SET service=''`},
		{"empty fingerprint", `UPDATE incidents SET fingerprint=''`},
		{"unknown symptom", `UPDATE incidents SET symptom='performance'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openCaptureStore(t, t.TempDir())
			seedRecall(t, s, recallFixtures(t)[:1])
			if _, err := s.DB().ExecContext(t.Context(), "PRAGMA ignore_check_constraints=ON"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().ExecContext(t.Context(), tc.sql); err != nil {
				t.Fatal(err)
			}
			got, err := New(s, nil).Recall(t.Context(), recallOptions())
			if !errors.Is(err, ledger.ErrCorrupt) || got.Message == "no incident history" || len(got.Matches) != 0 {
				t.Fatalf("corrupt storage must fail explicitly: result=%v err=%v", got, err)
			}
		})
	}
	t.Run("row iteration failure", func(t *testing.T) {
		s := openCaptureStore(t, t.TempDir())
		seedRecall(t, s, recallFixtures(t)[:1])
		if _, err := s.DB().ExecContext(t.Context(), `CREATE TEMP VIEW incidents AS
SELECT * FROM main.incidents UNION ALL
SELECT run_id,through_seq,captured_at,service,symptom,fingerprint,json('invalid') FROM main.incidents`); err != nil {
			t.Fatal(err)
		}
		got, err := New(s, nil).Recall(t.Context(), recallOptions())
		if err == nil || len(got.Matches) != 0 || got.Message == "no incident history" {
			t.Fatalf("late database failure returned partial or empty success: %v %v", got, err)
		}
	})
	t.Run("closed database", func(t *testing.T) {
		s := openCaptureStore(t, t.TempDir())
		service := New(s, nil)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := service.Recall(t.Context(), recallOptions())
		if err == nil || got.Message == "no incident history" {
			t.Fatalf("database failure became successful history: %v %v", got, err)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		s := openCaptureStore(t, t.TempDir())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := New(s, nil).Recall(ctx, recallOptions()); !errors.Is(err, context.Canceled) {
			t.Fatalf("want cancellation, got %v", err)
		}
	})
}

func TestRecallInputSafetyAndReadOnly(t *testing.T) {
	s := openCaptureStore(t, t.TempDir())
	l := seedCapture(t, s, captureFixture())
	want, err := New(s, captureNow).Capture(t.Context(), captureOptions())
	if err != nil {
		t.Fatal(err)
	}
	appendCaptureEntry(t, l, fixtureEntry{"later-criteria", ledger.KindExpectedBehavior, captureFixture()[3].payload})
	run, err := l.Load(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ValidateCapture(run); !errors.Is(err, ledger.ErrNotValidated) {
		t.Fatalf("later criteria must invalidate current capture eligibility: %v", err)
	}
	for _, tc := range []struct {
		name, message string
		change        func(*RecallOptions)
	}{
		{"service", "service", func(o *RecallOptions) { o.Service = " " }},
		{"fingerprint", "fingerprint", func(o *RecallOptions) { o.Fingerprint = "\n" }},
		{"UTF-8", "service", func(o *RecallOptions) { o.Service = string([]byte{0xff}) }},
		{"top", "top", func(o *RecallOptions) { o.Top = -1 }},
		{"symptom", "crashloop, latency, errors, metrics, oom, networking, availability", func(o *RecallOptions) { o.Symptom = "performance" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := recallOptions()
			tc.change(&opts)
			if _, err := New(s, nil).Recall(t.Context(), opts); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want input rejection containing %q, got %v", tc.message, err)
			}
		})
	}
	t.Run("shared byte bound", func(t *testing.T) {
		opts := recallOptions()
		opts.Fingerprint = strings.Repeat("x", safety.MaxResponseBytes+1)
		if _, err := New(s, nil).Recall(t.Context(), opts); !errors.Is(err, safety.ErrResponseTooLarge) {
			t.Fatalf("want shared response bound, got %v", err)
		}
	})
	t.Run("read only", func(t *testing.T) {
		if _, err := s.DB().ExecContext(t.Context(), "PRAGMA query_only=ON"); err != nil {
			t.Fatal(err)
		}
		var before, after int
		if err := s.DB().QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&before); err != nil {
			t.Fatal(err)
		}
		got, err := New(s, func() time.Time { t.Fatal("recall used capture clock"); return time.Time{} }).Recall(t.Context(), recallOptions())
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Matches) != 1 || !reflect.DeepEqual(got.Matches[0].Incident, want) {
			t.Fatalf("recall did not preserve captured record: %v", got)
		}
		if err := s.DB().QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&after); err != nil {
			t.Fatal(err)
		}
		if before != after || !reflect.DeepEqual(incidentRow(t, s), want) {
			t.Fatal("recall changed persisted state")
		}
	})
}
