package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func roundUpdate(number int, state, assessment string, evidence ...string) MetadataUpdate {
	if evidence == nil {
		evidence = []string{}
	}
	return MetadataUpdate{Investigation: &InvestigationUpdate{Number: number, State: state, Assessment: assessment, Reason: "distinguish queue saturation from network loss", Checks: []string{"compare queue depth and packet loss"}, EvidenceIDs: evidence, Questions: []string{"which constraint changed first?"}}}
}

func TestInvestigationRoundLimitResumeAndReplay(t *testing.T) {
	root := t.TempDir()
	s, db := openLedger(t, root)
	startLedger(t, s, "rounds", "")
	for n := range 5 {
		start := roundUpdate(n, "start", "")
		entry := addLedger(t, s, "rounds", fmt.Sprintf("start%d", n), KindMetadata, start)
		resumed, _ := openLedger(t, root)
		run, err := resumed.Load(t.Context(), "rounds")
		if err != nil || run.State.Investigation == nil || !run.State.Investigation.Active || run.State.Investigation.Number != n || run.State.Investigation.StartSeq != entry.Seq {
			t.Fatalf("lost active round: %+v %v", run.State.Investigation, err)
		}
		if replay := addLedger(t, resumed, "rounds", entry.ID, KindMetadata, start); !reflect.DeepEqual(replay, entry) {
			t.Fatal("start replay changed receipt")
		}
		evidence := fmt.Sprintf("e%d", n)
		addLedger(t, s, "rounds", evidence, KindEvidence, Evidence{Fact: "observed queue depth", Source: "metrics"})
		finish := roundUpdate(n, "finish", "insufficient", evidence)
		end := addLedger(t, s, "rounds", fmt.Sprintf("finish%d", n), KindMetadata, finish)
		if replay := addLedger(t, s, "rounds", end.ID, KindMetadata, finish); !reflect.DeepEqual(replay, end) {
			t.Fatal("finish replay changed receipt")
		}
	}
	run, err := s.Load(t.Context(), "rounds")
	if err != nil || run.State.Investigation.Active || run.State.Investigation.StopReason != "round_limit" || run.ThroughSeq != 15 {
		t.Fatalf("wrong limit: %+v %v", run, err)
	}
	before := ledgerInt(t, db.DB(), "SELECT next_seq FROM runs WHERE id='rounds'")
	raw, _ := json.Marshal(roundUpdate(5, "start", ""))
	if _, err := s.Add(t.Context(), AddRequest{RunID: "rounds", ID: "extra", Kind: KindMetadata, Payload: raw}); err == nil {
		t.Fatal("sixth round accepted")
	}
	if ledgerInt(t, db.DB(), "SELECT next_seq FROM runs WHERE id='rounds'") != before {
		t.Fatal("rejected round consumed sequence")
	}
	if EvaluateValidation(run).Verdict == "resolved" {
		t.Fatal("budget exhaustion claimed resolution")
	}
	view, err := Render(run, RenderOptions{Compact: true})
	if err != nil || !strings.Contains(view.Text, "round_limit") || !strings.Contains(view.Text, "which constraint changed first?") {
		t.Fatal("resume view lost round state", err, view.Text)
	}
}

func TestInvestigationEarlyStopsAndFreshEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, assessment, gap, stop string
		additional, fresh           bool
	}{
		{"sufficient", "sufficient", "", "sufficient_evidence", false, false},
		{"explicit-stop", "no_useful_data", "", "no_useful_data", false, false},
		{"gap-only", "insufficient", "metrics unavailable", "no_useful_data", false, false},
		{"stale", "insufficient", "", "no_useful_data", true, false},
		{"fresh", "insufficient", "", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openLedger(t, t.TempDir())
			startLedger(t, s, "r", "")
			addLedger(t, s, "r", "old", KindEvidence, Evidence{Fact: "triage", Source: "metrics", Gap: tc.gap})
			addLedger(t, s, "r", "start0", KindMetadata, roundUpdate(0, "start", ""))
			n := 0
			if tc.additional {
				addLedger(t, s, "r", "finish0", KindMetadata, roundUpdate(0, "finish", "insufficient", "old"))
				n = 1
				addLedger(t, s, "r", "start1", KindMetadata, roundUpdate(1, "start", ""))
			}
			ref := "old"
			if tc.fresh {
				ref = "fresh"
				addLedger(t, s, "r", ref, KindEvidence, Evidence{Fact: "new measurements", Source: "metrics"})
			}
			addLedger(t, s, "r", "finish", KindMetadata, roundUpdate(n, "finish", tc.assessment, ref))
			run, err := s.Load(t.Context(), "r")
			if err != nil || run.State.Investigation.StopReason != tc.stop || run.State.Investigation.Active {
				t.Fatalf("wrong stop state: %+v %v", run.State.Investigation, err)
			}
			if tc.stop != "" {
				raw, _ := json.Marshal(roundUpdate(n+1, "start", ""))
				if _, err := s.Add(t.Context(), AddRequest{RunID: "r", ID: "restart", Kind: KindMetadata, Payload: raw}); err == nil {
					t.Fatal("reopened stopped investigation")
				}
			}
		})
	}
}

func TestInvestigationRejectsInvalidTransitionsAtomically(t *testing.T) {
	s, db := openLedger(t, t.TempDir())
	startLedger(t, s, "r", "")
	startLedger(t, s, "other", "")
	addLedger(t, s, "other", "foreign", KindEvidence, Evidence{Fact: "foreign", Source: "metrics"})
	reject := func(update MetadataUpdate) {
		t.Helper()
		before := ledgerInt(t, db.DB(), "SELECT next_seq FROM runs WHERE id='r'")
		refs := ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_refs")
		raw, _ := json.Marshal(update)
		if _, err := s.Add(t.Context(), AddRequest{RunID: "r", ID: "invalid", Kind: KindMetadata, Payload: raw}); err == nil {
			t.Fatalf("accepted %+v", update.Investigation)
		}
		if ledgerInt(t, db.DB(), "SELECT next_seq FROM runs WHERE id='r'") != before || ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_refs") != refs {
			t.Fatal("partial write")
		}
	}
	reject(roundUpdate(0, "finish", "insufficient"))
	reject(roundUpdate(1, "start", ""))
	addLedger(t, s, "r", "start", KindMetadata, roundUpdate(0, "start", ""))
	reject(roundUpdate(0, "start", ""))
	reject(roundUpdate(1, "start", ""))
	reject(roundUpdate(1, "finish", "no_useful_data"))
	reject(roundUpdate(0, "finish", "sufficient"))
	for _, ref := range []string{"foreign", "missing", "start", "invalid"} {
		reject(roundUpdate(0, "finish", "insufficient", ref))
	}
	addLedger(t, s, "r", "gap", KindEvidence, Evidence{Fact: "no access", Source: "metrics", Gap: "access denied"})
	reject(roundUpdate(0, "finish", "sufficient", "gap"))
	addLedger(t, s, "r", "end", KindMetadata, roundUpdate(0, "finish", "insufficient", "gap"))
	reject(roundUpdate(0, "finish", "insufficient", "gap"))
	if ledgerInt(t, db.DB(), "SELECT count(*) FROM ledger_refs WHERE relation='investigation-evidence'") != 1 {
		t.Fatal("missing durable reference")
	}
}

func TestInvestigationShapeAndIdentity(t *testing.T) {
	for _, raw := range []string{
		`{"investigation":{"number":0,"state":"start","reason":"why","checks":[],"evidence_ids":[],"questions":[]}}`,
		`{"investigation":{"number":0,"state":"start","reason":" ","checks":["check"],"evidence_ids":[],"questions":[]}}`,
		`{"investigation":{"number":0,"state":"finish","assessment":"maybe","reason":"why","checks":["check"],"evidence_ids":[],"questions":[]}}`,
		`{"investigation":{"number":0,"state":"start","assessment":"sufficient","reason":"why","checks":["check"],"evidence_ids":[],"questions":[]}}`,
		`{"investigation":{"state":"start","reason":"why","checks":["check"],"evidence_ids":[],"questions":[]}}`,
	} {
		if _, err := DecodePayload(KindMetadata, json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	s, _ := openLedger(t, t.TempDir())
	startLedger(t, s, "r", "")
	id := "ghp_" + strings.Repeat("a", 36)
	addLedger(t, s, "r", id, KindEvidence, Evidence{Fact: "metrics", Source: "query"})
	addLedger(t, s, "r", "start", KindMetadata, roundUpdate(0, "start", ""))
	addLedger(t, s, "r", "finish", KindMetadata, roundUpdate(0, "finish", "sufficient", id))
	run, err := s.Load(t.Context(), "r")
	if err != nil || run.State.Investigation.EvidenceIDs[0] != id {
		t.Fatal("redaction changed evidence identity", err)
	}
	entries := append([]Entry(nil), run.Entries...)
	entries[2].Payload, _ = json.Marshal(roundUpdate(1, "finish", "sufficient", id))
	if _, err := Reduce(run.ID, run.Problem, run.CreatedAt, entries); err == nil {
		t.Fatal("reducer accepted corrupt transition history")
	}
}

func TestConcurrentInvestigationStartsHaveOneWinner(t *testing.T) {
	root := t.TempDir()
	s1, db := openLedger(t, root)
	s2, _ := openLedger(t, root)
	startLedger(t, s1, "r", "")
	raw, _ := json.Marshal(roundUpdate(0, "start", ""))
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, s := range []*Service{s1, s2} {
		wg.Go(func() {
			_, err := s.Add(t.Context(), AddRequest{RunID: "r", ID: fmt.Sprintf("start%d", i), Kind: KindMetadata, Payload: raw})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrInvalidInvestigation) {
			t.Fatal(err)
		}
	}
	if winners != 1 || ledgerInt(t, db.DB(), "SELECT next_seq FROM runs WHERE id='r'") != 2 {
		t.Fatal("concurrent starts consumed extra round")
	}
}
