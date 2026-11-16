package report

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
)

type snapshotReader struct{ run ledger.Run }

func (r snapshotReader) Load(context.Context, string) (ledger.Run, error) { return r.run, nil }

func snapshotFor(t *testing.T, run ledger.Run, opts SnapshotOptions) Snapshot {
	t.Helper()
	opts.Now = func() time.Time { return reportTime.Add(2 * time.Hour) }
	snapshot, err := GenerateSnapshot(t.Context(), snapshotReader{run}, run.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestDraftSnapshotStagesNeverImplyResolution(t *testing.T) {
	for _, stage := range []string{"interim", "post-incident"} {
		for _, complete := range []bool{false, true} {
			entries := validatedEntries(t)
			if !complete {
				entries = entries[:8]
			}
			run := runFrom(t, entries)
			before, _ := json.Marshal(run)
			snapshot := snapshotFor(t, run, SnapshotOptions{Stage: stage})
			want := "unresolved"
			if complete {
				want = "resolved"
			}
			if !snapshot.Draft || snapshot.Stage != stage || snapshot.Status != want || snapshot.Detail != "" {
				t.Fatalf("wrong snapshot state: %+v", snapshot)
			}
			for _, value := range []string{"Draft", "human review", "run", "2026-11-12T12:00:00Z", "Checkout", "Validation", want} {
				if !strings.Contains(snapshot.Markdown, value) {
					t.Errorf("missing %q: %s", value, snapshot.Markdown)
				}
			}
			after, _ := json.Marshal(run)
			if string(before) != string(after) {
				t.Fatal("projection mutated run")
			}
		}
	}
}

func TestSnapshotRetainsSourceGapsBeyondEvidenceExcerpt(t *testing.T) {
	entries := []ledger.Entry{}
	for i := range 10 {
		gap := ""
		if i == 9 {
			gap = "logs denied; cannot compare failed requests password=secret-value"
		}
		addEntry(t, &entries, fmt.Sprintf("source%d", i), ledger.KindEvidence, ledger.Evidence{Fact: "collection result", Source: "logs", Gap: gap})
	}
	snapshot := snapshotFor(t, runFrom(t, entries), SnapshotOptions{Detail: true})
	for _, content := range []string{snapshot.Markdown, snapshot.Detail, strings.Join(snapshot.Gaps, " ")} {
		if !strings.Contains(content, "logs denied; cannot compare failed requests") || strings.Contains(content, "secret-value") {
			t.Fatal("source gap missing or unredacted", content)
		}
	}
}

func TestSnapshotInvestigationStopsRetainUncertainty(t *testing.T) {
	entries := validatedEntries(t)[:4]
	for _, id := range []string{"network", "quota"} {
		addEntry(t, &entries, id, ledger.KindHypothesis, ledger.Hypothesis{Statement: id + " possible cause", Mechanism: "requests delayed", Confidence: "low", Status: "open", Supporting: []string{}, Contradicting: []string{}, DiscriminatingTest: "compare measurements"})
	}
	for _, state := range []string{"start", "finish"} {
		assessment := ""
		if state == "finish" {
			assessment = "no_useful_data"
		}
		addEntry(t, &entries, state, ledger.KindMetadata, ledger.MetadataUpdate{Investigation: &ledger.InvestigationUpdate{Number: 0, State: state, Assessment: assessment, Reason: "source access unavailable", Checks: []string{"inspect queue depth"}, EvidenceIDs: []string{}, Questions: []string{"which failure began first?"}}})
	}
	for _, stage := range []string{"interim", "post-incident"} {
		snapshot := snapshotFor(t, runFrom(t, entries), SnapshotOptions{Stage: stage, Detail: true})
		if snapshot.Status != "unresolved" {
			t.Fatal("stop implied resolution")
		}
		for _, value := range []string{"no_useful_data", "network possible cause", "quota possible cause", "which failure began first?", "inspect queue depth", "source access unavailable"} {
			if !strings.Contains(snapshot.Markdown, strings.ReplaceAll(value, "_", `\_`)) || !strings.Contains(snapshot.Detail, strings.ReplaceAll(value, "_", `\_`)) {
				t.Fatalf("lost %q", value)
			}
		}
	}
}

func TestDraftSnapshotKeepsAlternativesAndDetailedHistory(t *testing.T) {
	entries := validatedEntries(t)[:4]
	for _, id := range []string{"network", "quota"} {
		addEntry(t, &entries, id, ledger.KindHypothesis, ledger.Hypothesis{Statement: id + " alternative", Mechanism: "requests wait", Confidence: "low", Status: "open", Supporting: []string{"latency"}, Contradicting: []string{"latency"}, DiscriminatingTest: "check " + id})
	}
	addEntry(t, &entries, "proposal", ledger.KindOption, ledger.Option{Summary: "increase capacity", Steps: []string{}, Pros: []string{}, Cons: []string{}, Risk: "extra cost", Rollback: "restore capacity", HypothesisIDs: []string{"quota"}})
	snapshot := snapshotFor(t, runFrom(t, entries), SnapshotOptions{Detail: true})
	for _, value := range []string{"network alternative", "quota alternative", "Contradicting", "check quota", "increase capacity", "extra cost", "latency"} {
		if !strings.Contains(snapshot.Markdown, value) {
			t.Errorf("summary lost %q: %s", value, snapshot.Markdown)
		}
	}
	for _, entry := range entries {
		if !strings.Contains(snapshot.Detail, entry.ID) {
			t.Errorf("detail lost entry %s", entry.ID)
		}
	}
	if !strings.Contains(snapshot.Detail, "Draft") || !strings.Contains(snapshot.Detail, "2026-11-12T11:00:00Z") {
		t.Fatal("detail missing draft/provenance")
	}
}

func TestDraftSnapshotCustomRulesBeforeEscaping(t *testing.T) {
	entries := validatedEntries(t)
	addEntry(t, &entries, "private", ledger.KindEvidence, ledger.Evidence{Fact: "customer_ID=account_77 email=person@example.test password=hidden-value", Source: "sensitive incident notes"})
	snapshot := snapshotFor(t, runFrom(t, entries), SnapshotOptions{Detail: true, Rules: []Rule{
		{Name: "customer", Pattern: `account_77`, Action: "redact"},
		{Name: "email", Pattern: `person@example\.test`, Action: "redact"},
		{Name: "review-notes", Pattern: `sensitive incident notes`, Action: "flag"},
	}})
	for _, text := range []string{snapshot.Markdown, snapshot.Detail} {
		for _, secret := range []string{"account", "person", "hidden-value"} {
			if strings.Contains(text, secret) {
				t.Fatalf("export leaked %s", secret)
			}
		}
		if !strings.Contains(text, "review-notes") || !strings.Contains(text, "human review") {
			t.Fatal("sharing review flag absent")
		}
	}
	if !reflect.DeepEqual(snapshot.Flags, []string{"review-notes"}) {
		t.Fatal(snapshot.Flags)
	}
}

func TestDraftSnapshotLinksAndMissingEvidence(t *testing.T) {
	entries := []ledger.Entry{}
	addEntry(t, &entries, "source", ledger.KindEvidence, ledger.Evidence{Fact: "source unavailable", Source: "dashboard", Links: []string{
		"https://example.test/graphs?q=error", "https://example.test/?to%6ben=url-private", "javascript:alert(1)", "https://user:pass@example.test/", "https://example.test/a)b",
	}})
	snapshot := snapshotFor(t, runFrom(t, entries), SnapshotOptions{Detail: true})
	if !strings.Contains(snapshot.Markdown, "](https://example.test/graphs?q=error)") || !strings.Contains(snapshot.Markdown, "%29") {
		t.Fatal("working evidence links absent")
	}
	for _, content := range []string{snapshot.Markdown, snapshot.Detail} {
		if strings.Contains(content, "](javascript:") || strings.Contains(content, "url-private") || strings.Contains(content, "user:pass") {
			t.Fatalf("unsafe URL: %s", content)
		}
		if !strings.Contains(content, "not recorded") || !strings.Contains(content, "source unavailable") {
			t.Fatal("missing evidence hidden")
		}
	}
	if len(snapshot.Gaps) == 0 {
		t.Fatal("absence of expected validation was hidden")
	}
}

func TestDraftSnapshotRejectsInvalidOptions(t *testing.T) {
	run := runFrom(t, nil)
	for _, opts := range []SnapshotOptions{
		{Stage: "confirmed"}, {Now: func() time.Time { return time.Time{} }},
		{Rules: []Rule{{Name: "broken", Pattern: "[", Action: "redact"}}},
		{Rules: []Rule{{Name: "broken", Pattern: ".", Action: "allow"}}},
		{Rules: []Rule{{Name: "unsafe\nname", Pattern: ".", Action: "flag"}}},
		{Rules: make([]Rule, 65)},
	} {
		if _, err := GenerateSnapshot(t.Context(), snapshotReader{run}, run.ID, opts); err == nil {
			t.Fatalf("invalid options accepted: %+v", opts)
		}
	}
}

func TestSharingPolicyRequiresAnExplicitRulesObject(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"rules":null}`, `{"rules":[],"unknown":true}`, `{"rules":[]} {}`, strings.Repeat(" ", 256<<10) + `{"rules":[]}`} {
		if _, err := ReadPolicy(strings.NewReader(raw)); err == nil {
			t.Fatalf("invalid policy accepted: %.40q", raw)
		}
	}
	if rules, err := ReadPolicy(strings.NewReader(`{"rules":[]}`)); err != nil || len(rules) != 0 {
		t.Fatalf("empty explicit rules rejected: %v", err)
	}
}

func TestSnapshotRedactsEncodedCredentialsInsideFieldURLs(t *testing.T) {
	entries := validatedEntries(t)
	addEntry(t, &entries, "url", ledger.KindEvidence, ledger.Evidence{Fact: "See https://example.test/?to%6ben=encoded-private for evidence", Source: "https://example.test/?api%5fkey=source-private"})
	snapshot := snapshotFor(t, runFrom(t, entries), SnapshotOptions{Detail: true})
	for _, content := range []string{snapshot.Markdown, snapshot.Detail} {
		if strings.Contains(content, "encoded-private") || strings.Contains(content, "source-private") {
			t.Fatal("encoded URL credential leaked")
		}
	}
}

func TestUnvalidatedActionIsNotLabeledValidated(t *testing.T) {
	doc := snapshotFor(t, runFrom(t, validatedEntries(t)[:8]), SnapshotOptions{Stage: "post-incident"})
	if strings.Contains(doc.Markdown, "Validated action: action") || strings.Contains(doc.Markdown, "Validated fix: action") {
		t.Fatal("unvalidated action presented as validated")
	}
	if !strings.Contains(doc.Markdown, "Selected action: action") {
		t.Fatal("selected action provenance missing")
	}
}

func TestSnapshotURLRulesMatchOriginalAndDecodedValues(t *testing.T) {
	for _, rawURL := range []string{"https://example.test/?email=person@example.test", "https://example.test/?email=person%40example.test", "https://example.test/person%40example.test"} {
		var entries []ledger.Entry
		addEntry(t, &entries, "private-url", ledger.KindEvidence, ledger.Evidence{Fact: "check source", Source: rawURL, Links: []string{rawURL}})
		doc := snapshotFor(t, runFrom(t, entries), SnapshotOptions{Detail: true, Rules: []Rule{
			{Name: "review-email", Pattern: `person@example\.test`, Action: "flag"},
			{Name: "email", Pattern: `person@example\.test`, Action: "redact"},
		}})
		for _, content := range []string{doc.Markdown, doc.Detail} {
			if strings.Contains(content, "person") || strings.Contains(content, "](https://example.test/") {
				t.Fatalf("URL privacy rule bypass: %s", content)
			}
			if !strings.Contains(content, "review-email") {
				t.Fatal("URL flag missed original email")
			}
		}
	}
}

func TestSnapshotRedactsSensitiveURLQueryKeys(t *testing.T) {
	for _, query := range []string{"authorization=Bearer%20private-credential", "credential=private-credential", "api%5fkey=private-credential"} {
		var entries []ledger.Entry
		link := "https://example.test/?" + query
		addEntry(t, &entries, "url", ledger.KindEvidence, ledger.Evidence{Fact: "inspect " + link, Source: link, Links: []string{link}})
		doc := snapshotFor(t, runFrom(t, entries), SnapshotOptions{Detail: true})
		for _, content := range []string{doc.Markdown, doc.Detail} {
			if strings.Contains(content, "private-credential") || strings.Contains(content, "](https://example.test/") {
				t.Fatalf("URL credential exposed: %s", content)
			}
		}
	}
}
