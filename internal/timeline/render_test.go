package timeline

import (
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/safety"
)

func TestRenderText(t *testing.T) {
	wrapper, err := safety.NewWrapper()
	if err != nil {
		t.Fatal(err)
	}
	report := Report{Start: timelineScope().Start, End: timelineScope().End, Context: "prod", Namespace: "ns", Entries: []Entry{{Time: timelineScope().End, Source: "git", Object: Object{Kind: "GitCommit", Name: "abc"}, Change: "hostile text"}}, Sources: []SourceSummary{{Source: "flux-kustomizations", Status: "partial", Context: map[string]any{"revision": "undated-revision"}}}, Gaps: []Gap{{Source: "git", Code: "history", Message: "shallow history", Count: 1}}, Errors: []string{"request failed"}}
	text, err := RenderText(report, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{report.End.Format(time.RFC3339), "git", "GitCommit/abc", "hostile text", "undated-revision", "shallow history", "request failed", "BEGIN EXTERNAL DATA", "END EXTERNAL DATA"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if _, err = RenderText(report, nil); err == nil {
		t.Fatal("nil wrapper accepted")
	}
}
