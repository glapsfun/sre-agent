package timeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
)

func TestCollectSourcesAndDeadline(t *testing.T) {
	now := timelineScope().End
	var reads atomic.Int32
	var active atomic.Int32
	kube := streamKube{stream: func(ctx context.Context, w io.Writer, args ...string) executil.Result {
		active.Add(1)
		defer active.Add(-1)
		if args[1] == "events" {
			<-ctx.Done()
			return executil.Result{Err: ctx.Err()}
		}
		_, err := io.WriteString(w, eventList())
		return executil.Result{Err: err}
	}}
	runner := gitRunner{run: func(context.Context, string, ...string) executil.Result { return executil.Result{Stdout: "false"} }, stream: func(_ context.Context, w io.Writer, _ string, _ ...string) executil.Result {
		_, err := io.WriteString(w, strings.Repeat("a", 40)+"\x002026-11-11T11:00:00Z\x00commit\x00")
		return executil.Result{Err: err}
	}}
	httpClient := alertDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fixture(t, "alerts")))}, nil
	})
	for _, unavailable := range []bool{false, true} {
		reads.Store(0)
		deps := Dependencies{Kube: kube, Runner: runner, HTTP: httpClient, Now: func() time.Time { reads.Add(1); return now }}
		if unavailable {
			deps.Kube = nil
			deps.KubeError = errors.New("context unavailable token=hidden")
		}
		start := time.Now()
		report, err := Collect(context.Background(), Options{Namespace: "ns", Since: 2 * time.Hour, Timeout: 200 * time.Millisecond, Repo: "repo", AlertmanagerURL: "http://alerts.invalid"}, deps)
		if err != nil || reads.Load() != 1 || active.Load() != 0 || len(report.Sources) != 7 || len(report.Errors) == 0 || len(report.Entries) != 7 || !report.End.Equal(now) || report.End.Sub(report.Start) != 2*time.Hour || time.Since(start) > 2*time.Second {
			t.Fatalf("report=%+v err=%v reads=%d active=%d", report, err, reads.Load(), active.Load())
		}
		if !unavailable && report.Context != "pinned" {
			t.Fatalf("context=%q", report.Context)
		}
	}
	t.Run("unresolved workload", func(t *testing.T) {
		report, err := Collect(context.Background(), Options{Namespace: "ns", Workload: "deploy/missing", Since: 2 * time.Hour, Timeout: time.Second, AlertmanagerURL: "http://alerts.invalid"}, Dependencies{Kube: listKube(eventList()), HTTP: httpClient, Now: func() time.Time { return now }})
		if err != nil || len(report.Entries) != 0 || len(report.Sources) != 7 || len(report.Errors) == 0 {
			t.Fatalf("report=%+v err=%v", report, err)
		}
		counts := map[string]bool{}
		for _, g := range report.Gaps {
			if g.Code == "association" {
				counts[g.Source] = true
			}
		}
		if !counts["kubernetes-events"] || !counts["alertmanager"] {
			t.Fatalf("gaps=%+v", report.Gaps)
		}
	})
}

func TestCollectValidation(t *testing.T) {
	for _, opts := range []Options{{}, {Namespace: "ns", Since: -1}, {Namespace: "ns", Timeout: -1}, {Namespace: "ns", Workload: "service/api"}, {Namespace: "ns", Workload: "deploy/a/b"}} {
		_, err := Collect(context.Background(), opts, Dependencies{Kube: listKube(eventList())})
		if err == nil {
			t.Fatalf("accepted %+v", opts)
		}
	}
}

type pendingDeadline struct {
	context.Context
	deadline time.Time
}

func (c pendingDeadline) Deadline() (time.Time, bool) { return c.deadline, true }

func TestCollectRejectsExpiredDeadlineBeforeCancellation(t *testing.T) {
	ctx := pendingDeadline{context.Background(), time.Now().Add(-time.Second)}
	report, err := Collect(ctx, Options{Namespace: "ns", Since: 2 * time.Hour}, Dependencies{Kube: listKube(eventList(eventRecord("event"))), Now: func() time.Time { return timelineScope().End }})
	if err != nil || len(report.Entries) != 0 || len(report.Errors) == 0 {
		t.Fatalf("late records accepted: %+v err=%v", report, err)
	}
	for _, source := range report.Sources {
		if source.Source == "kubernetes-events" && source.Status != "timeout" {
			t.Fatalf("source=%+v", source)
		}
	}
}

func TestCollectSharesSetupBudgetAndJoinsCanceledWorkers(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	deadlines := make(chan time.Time, 10)
	var active atomic.Int32
	kube := streamKube{stream: func(ctx context.Context, w io.Writer, args ...string) executil.Result {
		active.Add(1)
		defer active.Add(-1)
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("missing deadline")
		}
		deadlines <- deadline
		if strings.Contains(args[1], "deployments.apps,") {
			time.Sleep(30 * time.Millisecond)
			_, err := io.WriteString(w, fixture(t, "owners"))
			return executil.Result{Err: err}
		}
		if args[1] == "events" {
			cancel()
			<-ctx.Done()
			return executil.Result{Err: ctx.Err()}
		}
		_, err := io.WriteString(w, eventList())
		return executil.Result{Err: err}
	}}
	report, err := Collect(parent, Options{Namespace: "ns", Workload: "deploy/api", Since: 2 * time.Hour, Timeout: time.Second}, Dependencies{Kube: kube, Now: func() time.Time { return timelineScope().End }})
	if err != nil || active.Load() != 0 || len(report.Sources) != 7 || len(report.Errors) == 0 {
		t.Fatalf("report=%+v err=%v active=%d", report, err, active.Load())
	}
	close(deadlines)
	first := time.Time{}
	for deadline := range deadlines {
		if first.IsZero() {
			first = deadline
		}
		if deadline != first {
			t.Fatalf("budget restarted: %s vs %s", first, deadline)
		}
	}
}

type countedContextKube struct {
	streamKube
	reads *atomic.Int32
}

func (k countedContextKube) Context() string { k.reads.Add(1); return "selected" }

func TestCollectKeepsPinnedContextAfterOwnershipTimeout(t *testing.T) {
	var reads atomic.Int32
	kube := countedContextKube{streamKube: streamKube{stream: func(ctx context.Context, _ io.Writer, _ ...string) executil.Result {
		<-ctx.Done()
		return executil.Result{Err: ctx.Err()}
	}}, reads: &reads}
	report, err := Collect(context.Background(), Options{Namespace: "ns", Workload: "deploy/api", Timeout: 20 * time.Millisecond}, Dependencies{Kube: kube})
	if err != nil || report.Context != "selected" || reads.Load() != 1 || len(report.Errors) == 0 || len(report.Entries) != 0 {
		t.Fatalf("report=%+v err=%v reads=%d", report, err, reads.Load())
	}
}

func TestCollectUnresolvedWorkloadRetainsMissingEndpointGap(t *testing.T) {
	t.Setenv("SRE_ALERTMANAGER_URL", "")
	report, err := Collect(context.Background(), Options{Namespace: "ns", Workload: "deploy/api"}, Dependencies{KubeError: errors.New("no Kubernetes context")})
	if err != nil {
		t.Fatal(err)
	}
	missing, association := false, false
	for _, gap := range report.Gaps {
		if gap.Source == "alertmanager" {
			missing = missing || gap.Code == "missing"
			association = association || gap.Code == "association"
		}
	}
	if !missing || !association {
		t.Fatalf("gaps=%+v", report.Gaps)
	}
}

func TestCollectSourceProcessingDeadline(t *testing.T) {
	t.Setenv("SRE_ALERTMANAGER_URL", "")
	large := strings.Repeat("x", 6<<20)
	event := eventRecord("large-event")
	event["message"] = large
	var list map[string]any
	if err := json.Unmarshal([]byte(fixture(t, "replicasets")), &list); err != nil {
		t.Fatal(err)
	}
	rs := list["items"].([]any)[1].(map[string]any)
	template := rs["spec"].(map[string]any)["template"].(map[string]any)
	template["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = large
	list["items"] = []any{rs}
	rsBody, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ resource, source, body string }{
		{"events", "kubernetes-events", eventList(event)},
		{"replicasets.apps", "replicasets", string(rsBody)},
	} {
		t.Run(tc.source, func(t *testing.T) {
			var active atomic.Int32
			kube := streamKube{stream: func(ctx context.Context, out io.Writer, args ...string) executil.Result {
				active.Add(1)
				defer active.Add(-1)
				body := eventList()
				if args[1] == tc.resource {
					body = tc.body
				}
				_, err := io.WriteString(out, body)
				return executil.Result{Err: err}
			}}
			runner := gitRunner{run: func(context.Context, string, ...string) executil.Result { return executil.Result{Stdout: "false"} }, stream: func(_ context.Context, out io.Writer, _ string, _ ...string) executil.Result {
				_, err := io.WriteString(out, strings.Repeat("a", 40)+"\x002026-11-11T11:00:00Z\x00independent commit\x00")
				return executil.Result{Err: err}
			}}
			start := time.Now()
			report, err := Collect(context.Background(), Options{Namespace: "ns", Repo: "repo", Timeout: time.Second}, Dependencies{Kube: kube, Runner: runner, Now: func() time.Time { return timelineScope().End }})
			elapsed := time.Since(start)
			if err != nil || active.Load() != 0 || len(report.Entries) != 1 || report.Entries[0].Source != "git" || report.Entries[0].Change != "independent commit" || len(report.Errors) == 0 {
				t.Fatalf("elapsed=%s report=%+v err=%v active=%d", elapsed, report, err, active.Load())
			}
			status := ""
			for _, source := range report.Sources {
				if source.Source == tc.source {
					status = source.Status
				}
			}
			if status != "timeout" || elapsed > 1300*time.Millisecond {
				t.Fatalf("source=%s status=%s elapsed=%s; one-second collection budget exceeded", tc.source, status, elapsed)
			}
			t.Logf("source=%s elapsed=%s; timed out batch discarded, independent git retained, workers joined", tc.source, elapsed)
		})
	}
}
