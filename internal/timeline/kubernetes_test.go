package timeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
)

func timelineScope() Scope {
	return Scope{Namespace: "ns", Context: "pinned", Start: time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
}
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, e := os.ReadFile("testdata/" + name + ".json")
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func eventRecord(uid string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Event", "metadata": map[string]any{"namespace": "ns", "name": uid, "uid": uid, "creationTimestamp": "2026-11-11T11:00:00Z"}, "involvedObject": map[string]any{"apiVersion": "v1", "kind": "Pod", "namespace": "ns", "name": "api-pod", "uid": "pod"}, "type": "Normal", "reason": "Started", "message": "same message", "firstTimestamp": "2026-11-11T09:00:00Z", "lastTimestamp": "2026-11-11T11:00:00Z", "count": 12}
}
func eventList(items ...any) string {
	b, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "EventList", "items": items})
	return string(b)
}
func hasGap(r SourceResult, code string) bool {
	for _, g := range r.Gaps {
		if g.Code == code && g.Count > 0 {
			return true
		}
	}
	return false
}

func TestAllEventsNotLastThirty(t *testing.T) {
	var items []any
	for i := 0; i < 120; i++ {
		e := eventRecord(fmt.Sprintf("event-%03d", i))
		e["message"] = strings.Repeat("event detail ", 7000)
		items = append(items, e)
	}
	body := eventList(items...)
	if len(body) <= 8<<20 {
		t.Fatal("fixture must exceed buffered response limit")
	}
	kube := streamKube{stream: func(_ context.Context, w io.Writer, args ...string) executil.Result {
		if strings.Join(args, " ") != "get events --namespace=ns --chunk-size=500 --output=json" {
			return executil.Result{Err: fmt.Errorf("unexpected command: %v", args)}
		}
		_, e := io.WriteString(w, body)
		return executil.Result{Err: e}
	}}
	r := CollectEvents(context.Background(), timelineScope(), kube)
	report := Merge(timelineScope(), []SourceResult{r})
	if len(report.Entries) != 120 || len(r.Errors) != 0 || r.Summary.Observed != 120 {
		t.Fatalf("entries=%d summary=%+v errors=%v", len(report.Entries), r.Summary, r.Errors)
	}
	for _, e := range report.Entries {
		if e.Details["type"] != "Normal" {
			t.Fatalf("lost Normal event: %+v", e)
		}
	}
}

func TestEventTimesAndFailures(t *testing.T) {
	t.Run("aggregate and fallbacks", func(t *testing.T) {
		r := CollectEvents(context.Background(), timelineScope(), listKube(fixture(t, "events")))
		if len(r.Entries) != 5 || r.Summary.Observed != 7 || !hasGap(r, "timestamp") || !hasGap(r, "error") {
			t.Fatalf("result=%+v", r)
		}
		want := []string{"series.lastObservedTime", "lastTimestamp", "eventTime", "firstTimestamp", "metadata.creationTimestamp"}
		for i, e := range r.Entries {
			if e.TimeBasis != want[i] || !e.Time.Equal(time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC)) {
				t.Errorf("entry=%+v", e)
			}
		}
		b, _ := json.Marshal(r.Entries[0].Details)
		for _, s := range []string{"2026-11-11T09:00:00Z", "2026-11-11T11:00:00Z", "12"} {
			if !strings.Contains(string(b), s) {
				t.Fatalf("aggregate missing %s: %s", s, b)
			}
		}
	})
	t.Run("exact scope", func(t *testing.T) {
		scope, _, err := ResolveScope(context.Background(), Options{Namespace: "ns", Workload: "deploy/api"}, listKube(fixture(t, "owners")))
		if err != nil {
			t.Fatal(err)
		}
		scope.Start = timelineScope().Start
		scope.End = timelineScope().End
		ok := eventRecord("ok")
		bad := eventRecord("bad")
		bad["involvedObject"].(map[string]any)["uid"] = "recreated"
		fallback := eventRecord("fallback")
		delete(fallback["involvedObject"].(map[string]any), "uid")
		r := CollectEvents(context.Background(), scope, listKube(eventList(ok, bad, fallback)))
		if len(r.Entries) != 2 || !hasGap(r, "association") {
			t.Fatalf("result=%+v", r)
		}
		scope.Target = nil
		r = CollectEvents(context.Background(), scope, listKube(eventList(ok)))
		if len(r.Entries) != 0 {
			t.Fatal("unresolved workload broadened")
		}
	})
	for _, tc := range []struct {
		name, body, stderr, status string
		err                        error
	}{
		{"forbidden", eventList(eventRecord("ok")), "Error from server (Forbidden): events is forbidden", "denied", errors.New("exit status 1")},
		{"missing", eventList(eventRecord("ok")), "the server doesn't have a resource type events", "missing", errors.New("exit status 1")},
		{"timeout", eventList(eventRecord("ok")), "", "timeout", context.DeadlineExceeded},
		{"partial", strings.TrimSuffix(eventList(eventRecord("ok")), "}"), "", "error", nil},
		{"malformed", `{"items":[`, "", "error", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := streamKube{stream: func(_ context.Context, w io.Writer, _ ...string) executil.Result {
				_, _ = io.WriteString(w, tc.body)
				return executil.Result{Err: tc.err, Stderr: tc.stderr}
			}}
			r := CollectEvents(context.Background(), timelineScope(), k)
			if len(r.Entries) != 0 || len(r.Errors) == 0 || r.Summary.Status != tc.status || !hasGap(r, tc.status) {
				t.Fatalf("failed source=%+v", r)
			}
		})
	}
}

func TestReplicaSetImagesAndSpecChanges(t *testing.T) {
	r := CollectReplicaSets(context.Background(), timelineScope(), listKube(fixture(t, "replicasets")))
	if len(r.Errors) != 0 || len(r.Entries) != 4 {
		t.Fatalf("result=%+v", r)
	}
	byID := map[string]Entry{}
	for _, e := range r.Entries {
		byID[e.ID] = e
		if e.TimeBasis != "replicaset-created" {
			t.Fatal(e.TimeBasis)
		}
	}
	a := byID["a2"]
	b, _ := json.Marshal(a.Details)
	for _, want := range []string{"app:v1", "app:v2", "spec.containers[api].image", "100m", "200m", "failureThreshold", "config-v2"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing delta %s in %s", want, b)
		}
	}
	if strings.Contains(string(b), "other:") {
		t.Fatal("compared deployments")
	}
	b, _ = json.Marshal(byID["b2"].Details)
	if !strings.Contains(string(b), "other:v1") || strings.Contains(string(b), "app:v") {
		t.Fatalf("wrong second baseline: %s", b)
	}
	if !hasGap(r, "history") || !hasGap(r, "timestamp") || !hasGap(r, "redacted") {
		t.Fatalf("missing uncertainty: %+v", r.Gaps)
	}
	output, _ := json.Marshal(r)
	if strings.Contains(string(output), "CANARY") {
		t.Fatal("secret escaped source")
	}
	s := timelineScope()
	s.Workload = "statefulset/db"
	s.Target = &Object{Kind: "StatefulSet", Name: "db"}
	r = CollectReplicaSets(context.Background(), s, listKube(fixture(t, "replicasets")))
	if len(r.Entries) != 0 || !hasGap(r, "history") {
		t.Fatalf("non Deployment=%+v", r)
	}
}

func TestEventTimesInvalidEndpointsAndBounds(t *testing.T) {
	e := eventRecord("invalid-first")
	e["firstTimestamp"] = "invalid"
	r := CollectEvents(context.Background(), timelineScope(), listKube(eventList(e)))
	if len(r.Entries) != 1 || !hasGap(r, "timestamp") {
		t.Fatalf("invalid endpoint silently omitted: %+v", r)
	}
	e = eventRecord("fallback")
	e["series"] = map[string]any{"lastObservedTime": "invalid", "count": 12}
	r = CollectEvents(context.Background(), timelineScope(), listKube(eventList(e)))
	if len(r.Entries) != 1 || r.Entries[0].TimeBasis != "lastTimestamp" || !hasGap(r, "timestamp") {
		t.Fatalf("fallback result=%+v", r)
	}
	start := eventRecord("start")
	start["lastTimestamp"] = "2026-11-11T10:00:00Z"
	end := eventRecord("end")
	end["lastTimestamp"] = "2026-11-11T12:00:00Z"
	future := eventRecord("future")
	future["lastTimestamp"] = "2026-11-11T13:00:00Z"
	r = CollectEvents(context.Background(), timelineScope(), listKube(eventList(start, end, future)))
	if len(r.Entries) != 2 || !hasGap(r, "timestamp") {
		t.Fatalf("window result=%+v", r)
	}
}

func TestEventTimesDiagnosticsStayRedactedAndUnwrapped(t *testing.T) {
	k := streamKube{stream: func(_ context.Context, w io.Writer, _ ...string) executil.Result {
		_, _ = io.WriteString(w, eventList())
		return executil.Result{Err: errors.New("exit status 1"), Stderr: "Forbidden: password=CANARY"}
	}}
	r := CollectEvents(context.Background(), timelineScope(), k)
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "CANARY") || strings.Contains(string(b), "BEGIN EXTERNAL DATA") || !strings.Contains(string(b), "Forbidden") {
		t.Fatalf("diagnostics boundary=%s", b)
	}
}

func TestReplicaSetFailuresAndExactOwnership(t *testing.T) {
	body := fixture(t, "replicasets")
	for _, tc := range []struct {
		name, body     string
		err            error
		stderr, status string
	}{
		{"partial", strings.TrimSuffix(strings.TrimSpace(body), "}"), nil, "", "error"},
		{"denied", body, errors.New("exit status 1"), "Forbidden", "denied"},
		{"timeout", body, context.DeadlineExceeded, "", "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := streamKube{stream: func(_ context.Context, w io.Writer, _ ...string) executil.Result {
				_, _ = io.WriteString(w, tc.body)
				return executil.Result{Err: tc.err, Stderr: tc.stderr}
			}}
			r := CollectReplicaSets(context.Background(), timelineScope(), k)
			if len(r.Entries) != 0 || len(r.Errors) == 0 || r.Summary.Status != tc.status {
				t.Fatalf("result=%+v", r)
			}
		})
	}
	var data struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		t.Fatal(err)
	}
	target := Object{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "ns", Name: "api", UID: "a"}
	scope := timelineScope()
	scope.Workload = "deploy/api"
	scope.Target = &target
	scope.Objects = map[string]Object{}
	scope.Related = map[string]bool{}
	for _, id := range []string{"a1", "a2"} {
		scope.Objects[id] = Object{APIVersion: "apps/v1", Kind: "ReplicaSet", Namespace: "ns", Name: id, UID: id}
		scope.Related[id] = true
	}
	r := CollectReplicaSets(context.Background(), scope, listKube(body))
	if len(r.Entries) != 1 || r.Entries[0].ID != "a2" {
		t.Fatalf("exact scope=%+v", r)
	}
	changed := strings.ReplaceAll(body, `"uid": "a"`, `"uid": "recreated"`)
	r = CollectReplicaSets(context.Background(), scope, listKube(changed))
	if len(r.Entries) != 0 || !hasGap(r, "association") {
		t.Fatalf("recreated owner=%+v", r)
	}
}

func TestReplicaSetReuseAndMalformedPeer(t *testing.T) {
	var data struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(fixture(t, "replicasets")), &data); err != nil {
		t.Fatal(err)
	}
	data.Items[1]["metadata"].(map[string]any)["annotations"].(map[string]any)["deployment.kubernetes.io/revision-history"] = "1"
	items := []any{}
	for _, item := range data.Items {
		items = append(items, item)
	}
	items = append(items, map[string]any{"metadata": 42})
	r := CollectReplicaSets(context.Background(), timelineScope(), listKube(eventList(items...)))
	if len(r.Entries) != 4 || !hasGap(r, "error") {
		t.Fatalf("malformed peer=%+v", r)
	}
	found := false
	for _, gap := range r.Gaps {
		if strings.Contains(gap.Message, "indicates reuse") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no explicit reuse gap: %+v", r.Gaps)
	}
	for _, entry := range r.Entries {
		if entry.ID == "unknown" && entry.Details["baseline_available"] != false {
			t.Fatalf("unknown revision baseline=%+v", entry)
		}
	}
}
