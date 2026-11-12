package timeline

import (
	"reflect"
	"testing"
	"time"
)

func TestMergeSourceResults(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	scope := Scope{Start: now.Add(-time.Hour), End: now, Context: "pinned", Namespace: "ns", Workload: "api"}
	a := SourceResult{Summary: SourceSummary{Source: "kubernetes-events", Status: "ok", Observed: 2, Matched: 2}, Entries: []Entry{
		{ID: "uid-b", Time: now, Source: "kubernetes-events", Object: Object{Namespace: "ns", Kind: "Pod", Name: "api"}, Change: "same"},
		{ID: "uid-a", Time: now, Source: "kubernetes-events", Object: Object{Namespace: "ns", Kind: "Pod", Name: "api"}, Change: "same"},
	}, Gaps: []Gap{{Source: "kubernetes-events", Code: "history", Message: "retention"}}}
	b := SourceResult{Summary: SourceSummary{Source: "git", Status: "error"}, Entries: []Entry{{ID: "first", Time: scope.Start, Source: "git"}}, Gaps: []Gap{{Source: "git", Code: "error", Message: "failed"}}, Errors: []string{"git failed"}}
	r := Merge(scope, []SourceResult{a, b})
	if !reflect.DeepEqual(r, Merge(scope, []SourceResult{b, a})) {
		t.Fatal("result order changed report")
	}
	if len(r.Entries) != 3 || r.Entries[0].ID != "first" || r.Entries[1].ID != "uid-a" || r.Entries[2].ID != "uid-b" {
		t.Fatalf("entries=%+v", r.Entries)
	}
	if a.Entries[0].ID != "uid-b" {
		t.Fatal("merge mutated input")
	}
	if len(r.Sources) != 7 || len(r.Gaps) < 2 || len(r.Errors) != 1 || r.Context != "pinned" || r.Workload != "api" || r.Start != scope.Start {
		t.Fatalf("report=%+v", r)
	}
	seen := map[string]bool{}
	for _, summary := range r.Sources {
		seen[summary.Source] = true
	}
	for _, source := range []string{"kubernetes-events", "replicasets", "flux-kustomizations", "flux-helmreleases", "argocd", "git", "alertmanager"} {
		if !seen[source] {
			t.Errorf("missing source %s", source)
		}
	}
}

func TestMergeSourceResultsAllSortKeys(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	entries := []Entry{
		{ID: "id-2", Time: now, Source: "b", Object: Object{Namespace: "b", Kind: "Pod", Name: "b"}},
		{ID: "id-1", Time: now, Source: "b", Object: Object{Namespace: "b", Kind: "Pod", Name: "b"}},
		{ID: "name", Time: now, Source: "b", Object: Object{Namespace: "b", Kind: "Pod", Name: "a"}},
		{ID: "kind", Time: now, Source: "b", Object: Object{Namespace: "b", Kind: "Deployment", Name: "z"}},
		{ID: "namespace", Time: now, Source: "b", Object: Object{Namespace: "a", Kind: "Pod", Name: "z"}},
		{ID: "source", Time: now, Source: "a", Object: Object{Namespace: "z", Kind: "Pod", Name: "z"}},
		{ID: "time", Time: now.Add(-time.Second), Source: "z", Object: Object{Namespace: "z", Kind: "Pod", Name: "z"}},
	}
	report := Merge(Scope{}, []SourceResult{{Summary: SourceSummary{Source: "kubernetes-events", Status: "ok"}, Entries: entries}})
	var ids []string
	for _, entry := range report.Entries {
		ids = append(ids, entry.ID)
	}
	if !reflect.DeepEqual(ids, []string{"time", "source", "namespace", "kind", "name", "id-1", "id-2"}) {
		t.Fatalf("IDs=%v", ids)
	}
}
