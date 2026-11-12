package processlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/store"
)

func pointer[T any](v T) *T { return &v }
func testStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, id := range []string{"run-one", "run-two"} {
		if _, err := ledger.New(s, nil).Start(t.Context(), ledger.StartRequest{ID: id, Problem: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	return s, root
}
func event(id, kind string) Entry {
	e := Entry{RecordedAt: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC), RunID: "run-one", SessionID: "session-one", ToolUseID: id, Agent: "sre-k8s-investigator", Tool: "Bash", Command: "kubectl get pods", Event: kind}
	if kind == "pre" {
		e.GuardDecision = pointer("allowed")
	}
	return e
}
func calls(t *testing.T, s *store.Store, filter Filter) Report {
	t.Helper()
	r, err := Query(t.Context(), s, "run-one", filter)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestReconcileEvents(t *testing.T) {
	for _, tc := range []struct {
		name             string
		events           []Entry
		decision         *string
		outcome          string
		status, duration *int64
		conflicts        int
	}{
		{"pre allowed unknown", []Entry{event("a", "pre")}, pointer("allowed"), "unknown", nil, nil, 0},
		{"pre blocked unknown", []Entry{func() Entry { e := event("a", "pre"); e.GuardDecision = pointer("blocked"); return e }()}, pointer("blocked"), "unknown", nil, nil, 0},
		{"post succeeds without numeric status", []Entry{event("a", "pre"), event("a", "success"), event("a", "success"), event("a", "pre")}, pointer("allowed"), "succeeded", nil, nil, 0},
		{"post failure", []Entry{event("a", "failure")}, nil, "failed", nil, nil, 0},
		{"post before pre", []Entry{event("a", "success"), event("a", "pre")}, pointer("allowed"), "succeeded", nil, nil, 0},
		{"service numeric metrics", []Entry{event("a", "pre"), func() Entry {
			e := event("a", "failure")
			e.ExitStatus = pointer(int64(7))
			e.DurationMS = pointer(int64(30))
			return e
		}()}, pointer("allowed"), "failed", pointer(int64(7)), pointer(int64(30)), 0},
		{"contradictory terminal absorbing", []Entry{event("a", "success"), event("a", "failure"), event("a", "success")}, nil, "conflict", nil, nil, 2},
		{"blocked then success", []Entry{func() Entry { e := event("a", "pre"); e.GuardDecision = pointer("blocked"); return e }(), event("a", "success")}, pointer("blocked"), "conflict", nil, nil, 1},
		{"success then blocked", []Entry{event("a", "success"), func() Entry { e := event("a", "pre"); e.GuardDecision = pointer("blocked"); return e }()}, pointer("blocked"), "conflict", nil, nil, 1},
		{"opposite pre decision", []Entry{event("a", "pre"), func() Entry { e := event("a", "pre"); e.GuardDecision = pointer("blocked"); return e }()}, pointer("allowed"), "conflict", nil, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testStore(t)
			conflicts := 0
			for _, e := range tc.events {
				err := Record(t.Context(), s, e)
				if errors.Is(err, ErrConflict) {
					conflicts++
				} else if err != nil {
					t.Fatal(err)
				}
			}
			r := calls(t, s, Filter{})
			if len(r.Calls) != 1 {
				t.Fatalf("calls: %+v", r)
			}
			c := r.Calls[0]
			eq := func(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
			if c.Outcome != tc.outcome || (c.GuardDecision == nil) != (tc.decision == nil) || c.GuardDecision != nil && *c.GuardDecision != *tc.decision || !eq(c.ExitStatus, tc.status) || !eq(c.DurationMS, tc.duration) || conflicts != tc.conflicts {
				t.Fatalf("got %+v conflicts=%d", c, conflicts)
			}
		})
	}
}
func TestPinnedIdentityAndRun(t *testing.T) {
	for _, field := range []string{"run", "agent", "tool", "command", "path"} {
		t.Run(field, func(t *testing.T) {
			s, _ := testStore(t)
			e := event("a", "pre")
			if err := Record(t.Context(), s, e); err != nil {
				t.Fatal(err)
			}
			e.Event = "success"
			e.GuardDecision = nil
			switch field {
			case "run":
				e.RunID = "run-two"
			case "agent":
				e.Agent = "sre-logs-investigator"
			case "tool":
				e.Tool = "Read"
			case "command":
				e.Command = "kubectl get nodes"
			case "path":
				e.Path = "other"
			}
			if err := Record(t.Context(), s, e); !errors.Is(err, ErrConflict) {
				t.Fatalf("conflict: %v", err)
			}
			r := calls(t, s, Filter{})
			c := r.Calls[0]
			want := "conflict"
			if field == "run" {
				want = "unknown"
			}
			if len(r.Calls) != 1 || c.RunID != "run-one" || c.Agent != "sre-k8s-investigator" || c.Tool != "Bash" || c.Command != "kubectl get pods" || c.Path != "" || c.Outcome != want {
				t.Fatalf("identity changed: %+v", r)
			}
			r2, err := Query(t.Context(), s, "run-two", Filter{})
			if err != nil || len(r2.Calls) != 0 {
				t.Fatalf("reassigned: %+v %v", r2, err)
			}
		})
	}
}
func TestMetricEnrichmentAndConflict(t *testing.T) {
	s, _ := testStore(t)
	e := event("a", "success")
	if err := Record(t.Context(), s, e); err != nil {
		t.Fatal(err)
	}
	e.ExitStatus = pointer(int64(0))
	e.DurationMS = pointer(int64(12))
	for range 2 {
		if err := Record(t.Context(), s, e); err != nil {
			t.Fatal(err)
		}
	}
	c := calls(t, s, Filter{}).Calls[0]
	if c.ExitStatus == nil || *c.ExitStatus != 0 || c.DurationMS == nil || *c.DurationMS != 12 {
		t.Fatal(c)
	}
	e.DurationMS = pointer(int64(13))
	if err := Record(t.Context(), s, e); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	c = calls(t, s, Filter{}).Calls[0]
	if c.Outcome != "conflict" || c.ExitStatus != nil || c.DurationMS != nil {
		t.Fatal(c)
	}
}
func TestConcurrentSessionsAndDeliveries(t *testing.T) {
	s, root := testStore(t)
	other, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := event("shared", "pre")
			e.SessionID = fmt.Sprintf("session-%d", i%2)
			db := s
			if i%3 == 0 {
				db = other
			}
			errs <- Record(t.Context(), db, e)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if r := calls(t, s, Filter{}); len(r.Calls) != 2 {
		t.Fatalf("rows: %+v", r)
	}
}
func TestSanitizeBeforePersistence(t *testing.T) {
	s, root := testStore(t)
	e := event("a", "pre")
	secrets := []string{"planted-password", "planted-token", "planted-userpass", "planted-url", "planted-path"}
	e.Command = `curl --password 'planted-password' --token planted-token -u planted-userpass https://user:planted-url@example.test`
	e.Path = "/tmp/token=planted-path"
	if err := Record(t.Context(), s, e); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(calls(t, s, Filter{}))
	files, err := filepath.Glob(filepath.Join(root, ".sre-agent", "sre.db*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, b...)
	}
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("credential persisted: %s", secret)
		}
	}
	for _, field := range []string{"run", "session", "call", "agent", "tool"} {
		bad := event("b", "pre")
		switch field {
		case "run":
			bad.RunID = "token=planted-id"
		case "session":
			bad.SessionID = "token=planted-id"
		case "call":
			bad.ToolUseID = "token=planted-id"
		case "agent":
			bad.Agent = "token=planted-id:sre-k8s-investigator"
		case "tool":
			bad.Tool = "token=planted-id"
		}
		if err := Record(t.Context(), s, bad); err == nil {
			t.Fatalf("accepted unsafe %s identity", field)
		}
	}
}
func TestRecordValidation(t *testing.T) {
	s, _ := testStore(t)
	for _, field := range []string{"missing run", "invalid run", "session", "call", "agent", "tool", "time", "event", "decision", "pre metrics", "negative duration", "post decision"} {
		t.Run(field, func(t *testing.T) {
			e := event("a", "pre")
			switch field {
			case "missing run":
				e.RunID = "absent"
			case "invalid run":
				e.RunID = "../bad"
			case "session":
				e.SessionID = ""
			case "call":
				e.ToolUseID = ""
			case "agent":
				e.Agent = "main"
			case "tool":
				e.Tool = ""
			case "time":
				e.RecordedAt = time.Time{}
			case "event":
				e.Event = "other"
			case "decision":
				e.GuardDecision = nil
			case "pre metrics":
				e.ExitStatus = pointer(int64(0))
			case "negative duration":
				e.Event = "success"
				e.GuardDecision = nil
				e.DurationMS = pointer(int64(-1))
			case "post decision":
				e.Event = "success"
			}
			if err := Record(t.Context(), s, e); err == nil {
				t.Fatal("invalid event accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Record(ctx, s, event("a", "pre")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if r := calls(t, s, Filter{}); len(r.Calls) != 0 {
		t.Fatalf("invalid events stored: %+v", r)
	}
}

func TestWriterRestoresDurabilityOnReplacementConnections(t *testing.T) {
	s, _ := testStore(t)
	s.DB().SetMaxIdleConns(0)
	// Reopen with an idle connection whose settings differ from the ledger writer.
	s.DB().SetMaxIdleConns(1)
	if _, err := s.DB().Exec("PRAGMA foreign_keys=OFF; PRAGMA synchronous=NORMAL"); err != nil {
		t.Fatal(err)
	}
	if err := Record(t.Context(), s, event("a", "pre")); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{"foreign_keys": 1, "synchronous": 2} {
		var got int
		if err := s.DB().QueryRow("PRAGMA " + name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s=%d want %d", name, got, want)
		}
	}
}

func TestCredentialFlagFormsDoNotReachDatabase(t *testing.T) {
	s, root := testStore(t)
	for i, command := range []string{`curl --user=planted-basic https://example.test`, `curl -uplanted-attached https://example.test`, `curl --password='planted quoted' https://example.test`, `kubectl --token planted-separate get pods`, `curl -H 'Authorization: Bearer planted-header' https://example.test`} {
		e := event(fmt.Sprintf("flag-%d", i), "pre")
		e.Command = command
		if err := Record(t.Context(), s, e); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := json.Marshal(calls(t, s, Filter{}))
	files, _ := filepath.Glob(filepath.Join(root, ".sre-agent", "sre.db*"))
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, b...)
	}
	if strings.Contains(string(data), "planted") {
		t.Fatal("credential flag leaked")
	}
}
