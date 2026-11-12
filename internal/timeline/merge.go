package timeline

import (
	"cmp"
	"slices"
)

var sources = []string{"kubernetes-events", "replicasets", "flux-kustomizations", "flux-helmreleases", "argocd", "git", "alertmanager"}

func Merge(scope Scope, results []SourceResult) Report {
	report := Report{Start: scope.Start, End: scope.End, Context: scope.Context, Namespace: scope.Namespace, Workload: scope.Workload,
		Entries: []Entry{}, Sources: []SourceSummary{}, Gaps: []Gap{}, Errors: []string{}}
	seen := map[string]bool{}
	for _, result := range results {
		report.Sources = append(report.Sources, result.Summary)
		seen[result.Summary.Source] = true
		report.Entries = append(report.Entries, result.Entries...)
		report.Gaps = append(report.Gaps, result.Gaps...)
		report.Errors = append(report.Errors, result.Errors...)
	}
	for _, source := range sources {
		if !seen[source] {
			report.Sources = append(report.Sources, SourceSummary{Source: source, Status: "missing"})
			report.Gaps = append(report.Gaps, Gap{Source: source, Code: "missing", Message: "source result unavailable"})
		}
	}
	slices.SortStableFunc(report.Entries, func(a, b Entry) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.Object.Namespace, b.Object.Namespace), cmp.Compare(a.Object.Kind, b.Object.Kind), cmp.Compare(a.Object.Name, b.Object.Name), cmp.Compare(a.ID, b.ID))
	})
	slices.SortStableFunc(report.Sources, func(a, b SourceSummary) int { return cmp.Compare(a.Source, b.Source) })
	slices.SortStableFunc(report.Gaps, func(a, b Gap) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.Code, b.Code), cmp.Compare(a.Message, b.Message), cmp.Compare(a.Count, b.Count))
	})
	slices.Sort(report.Errors)
	return report
}
