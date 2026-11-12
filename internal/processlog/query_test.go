package processlog

import (
	"testing"
	"time"
)

func TestQueryFiltersStableOrderingAndUnknownMetrics(t *testing.T) {
	s, _ := testStore(t)
	for _, id := range []string{"c", "b", "a", "d"} {
		e := event(id, "pre")
		if id == "d" {
			e.GuardDecision = pointer("blocked")
		}
		if id == "b" {
			e.Agent = "sre-logs-investigator"
		}
		if err := Record(t.Context(), s, e); err != nil {
			t.Fatal(err)
		}
		if id == "a" || id == "b" {
			e.Event = "success"
			e.GuardDecision = nil
			e.DurationMS = pointer(int64(10))
			if err := Record(t.Context(), s, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	r := calls(t, s, Filter{Slowest: 3, ByInvestigator: true})
	if len(r.Calls) != 3 || r.Calls[0].ToolUseID != "a" || r.Calls[1].ToolUseID != "b" || r.Calls[2].ToolUseID != "c" || r.Calls[2].DurationMS != nil {
		t.Fatalf("order: %+v", r)
	}
	if len(r.Investigators) != 2 || r.Investigators[0].Agent != "sre-k8s-investigator" || r.Investigators[0].Calls != 2 || r.Investigators[0].Unknown != 1 || r.Investigators[0].Succeeded != 1 {
		t.Fatalf("groups: %+v", r.Investigators)
	}
	r = calls(t, s, Filter{Blocked: true, ByInvestigator: true})
	if len(r.Calls) != 1 || r.Calls[0].ToolUseID != "d" || r.Investigators[0].Blocked != 1 || r.Calls[0].Outcome != "unknown" {
		t.Fatalf("blocked: %+v", r)
	}
	r = calls(t, s, Filter{})
	for i, id := range []string{"a", "b", "c", "d"} {
		if r.Calls[i].ToolUseID != id {
			t.Fatal(r)
		}
	}
	for _, id := range []string{"missing", "../invalid"} {
		if _, err := Query(t.Context(), s, id, Filter{}); err == nil {
			t.Fatal("unknown/invalid run accepted")
		}
	}
	if _, err := Query(t.Context(), s, "run-one", Filter{Slowest: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
}

func TestObservationTimeOrderingAndFirstObservationPinned(t *testing.T) {
	s, _ := testStore(t)
	later := event("a", "success")
	later.RecordedAt = later.RecordedAt.Add(100 * time.Millisecond)
	earlier := event("b", "pre")
	for _, e := range []Entry{later, earlier} {
		if err := Record(t.Context(), s, e); err != nil {
			t.Fatal(err)
		}
	}
	replay := later
	replay.RecordedAt = replay.RecordedAt.Add(-time.Hour)
	replay.Event = "pre"
	replay.GuardDecision = pointer("allowed")
	if err := Record(t.Context(), s, replay); err != nil {
		t.Fatal(err)
	}
	r := calls(t, s, Filter{})
	if r.Calls[0].ToolUseID != "b" || r.Calls[1].ToolUseID != "a" || !r.Calls[1].RecordedAt.Equal(later.RecordedAt) {
		t.Fatalf("observation order/time: %+v", r.Calls)
	}
}
