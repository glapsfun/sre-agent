package timeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/glapsfun/sre-agent/internal/executil"
)

func ownerObject(kind, name, uid, ns, ownerKind, ownerName, ownerUID string) map[string]any {
	api := "apps/v1"
	if kind == "Pod" {
		api = "v1"
	}
	if kind == "Job" || kind == "CronJob" {
		api = "batch/v1"
	}
	metadata := map[string]any{"name": name, "uid": uid, "namespace": ns}
	if ownerUID != "" {
		ownerAPI := "apps/v1"
		if ownerKind == "Pod" {
			ownerAPI = "v1"
		}
		if ownerKind == "Job" || ownerKind == "CronJob" {
			ownerAPI = "batch/v1"
		}
		metadata["ownerReferences"] = []any{map[string]any{"apiVersion": ownerAPI, "kind": ownerKind, "name": ownerName, "uid": ownerUID, "controller": true}}
	}
	return map[string]any{"apiVersion": api, "kind": kind, "metadata": metadata}
}

func graphKube(objects ...map[string]any) streamKube {
	body, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": objects})
	return listKube(string(body))
}

func TestExactOwnerGraph(t *testing.T) {
	objects := []map[string]any{
		ownerObject("Deployment", "api", "d", "ns", "", "", ""),
		ownerObject("Deployment", "api-gateway", "gateway", "ns", "", "", ""),
		ownerObject("ReplicaSet", "api-rs", "rs", "ns", "Deployment", "api", "d"),
		ownerObject("Pod", "api-pod", "pod", "ns", "ReplicaSet", "api-rs", "rs"),
		ownerObject("Pod", "api-gateway-pod", "gateway-pod", "ns", "Deployment", "api-gateway", "gateway"),
		ownerObject("ReplicaSet", "old-api", "old", "ns", "Deployment", "api", "old-d"),
		ownerObject("Pod", "orphan", "orphan", "ns", "ReplicaSet", "gone", "missing"),
		ownerObject("Pod", "foreign", "foreign", "other", "ReplicaSet", "api-rs", "rs"),
		ownerObject("ReplicaSet", "wrong-kind", "wrong-kind", "ns", "Job", "api", "d"),
		ownerObject("ReplicaSet", "cycle-a", "a", "ns", "ReplicaSet", "cycle-b", "b"),
		ownerObject("ReplicaSet", "cycle-b", "b", "ns", "ReplicaSet", "cycle-a", "a"),
		ownerObject("Job", "api", "job", "ns", "", "", ""),
	}
	scope, gaps, err := ResolveScope(context.Background(), Options{Namespace: "ns", Workload: "deploy/api", Context: "ignored"}, graphKube(objects...))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for uid, yes := range scope.Related {
		if yes {
			got = append(got, uid)
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"d", "pod", "rs"}) || scope.Context != "pinned" {
		t.Fatalf("scope=%+v related=%v", scope, got)
	}
	if len(gaps) == 0 {
		t.Fatal("missing association gaps")
	}
	for _, gap := range gaps {
		if gap.Code != "association" {
			t.Errorf("gap=%+v", gap)
		}
	}
	for _, tc := range []struct {
		ref            Object
		want, fallback bool
	}{
		{Object{Namespace: "ns", Kind: "Pod", Name: "api-pod", UID: "pod"}, true, false},
		{Object{Namespace: "ns", Kind: "Pod", Name: "api-pod"}, true, true},
		{Object{Namespace: "ns", Kind: "Pod", Name: "api-pod", UID: "old-pod"}, false, false},
		{Object{Namespace: "ns", Kind: "Deployment", Name: "wrong", UID: "d"}, false, false},
		{Object{Namespace: "other", Kind: "Pod", Name: "api-pod", UID: "pod"}, false, false},
		{Object{Namespace: "ns", Kind: "Deployment", Name: "api-gateway"}, false, false},
	} {
		got, gap := scope.MatchObject(tc.ref)
		if got != tc.want || (gap != nil) != tc.fallback {
			t.Errorf("match %+v = %v,%+v", tc.ref, got, gap)
		}
	}
	_, _, err = ResolveScope(context.Background(), Options{Namespace: "ns", Workload: "api"}, graphKube(objects...))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguity error=%v", err)
	}
	for _, workload := range []string{"deploy/absent", "unknown/api", "api/too/many"} {
		failed, _, err := ResolveScope(context.Background(), Options{Namespace: "ns", Workload: workload}, graphKube(objects...))
		match, _ := failed.MatchObject(Object{Namespace: "ns", Kind: "Pod", Name: "unrelated"})
		if err == nil || failed.Workload != workload || match {
			t.Fatalf("failed lookup broadened scope: %+v error=%v", failed, err)
		}
	}
}

func TestExactOwnerGraphCronJobAndNamespace(t *testing.T) {
	kube := graphKube(ownerObject("CronJob", "scheduled", "cron", "ns", "", "", ""), ownerObject("Job", "run", "job", "ns", "CronJob", "scheduled", "cron"), ownerObject("Pod", "run-pod", "pod", "ns", "Job", "run", "job"))
	scope, _, err := ResolveScope(context.Background(), Options{Namespace: "ns", Workload: "cj/scheduled"}, kube)
	if err != nil || !scope.Related["pod"] || len(scope.Related) != 3 {
		t.Fatalf("scope=%+v err=%v", scope, err)
	}
	noReads := streamKube{stream: func(context.Context, io.Writer, ...string) executil.Result {
		t.Fatal("namespace request read ownership")
		return executil.Result{}
	}}
	scope, _, err = ResolveScope(context.Background(), Options{Namespace: "ns"}, noReads)
	matched, gap := scope.MatchObject(Object{Namespace: "ns", Kind: "Pod", Name: "any"})
	if err != nil || !matched || gap != nil {
		t.Fatalf("namespace match=%v gap=%v err=%v", matched, gap, err)
	}
}

func TestExactOwnerGraphFailedProducerDiscardsObjects(t *testing.T) {
	kube := streamKube{stream: func(_ context.Context, w io.Writer, _ ...string) executil.Result {
		body, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{ownerObject("Deployment", "api", "d", "ns", "", "", "")}})
		_, _ = w.Write(body)
		return executil.Result{Err: errors.New("denied")}
	}}
	scope, _, err := ResolveScope(context.Background(), Options{Namespace: "ns", Workload: "api"}, kube)
	if err == nil || scope.Target != nil || len(scope.Objects) != 0 || scope.Workload != "api" {
		t.Fatalf("scope=%+v err=%v", scope, err)
	}
}

func TestExactOwnerGraphVersionConversionAndCycles(t *testing.T) {
	deployment := ownerObject("Deployment", "api", "d", "ns", "", "", "")
	converted := ownerObject("ReplicaSet", "converted", "converted", "ns", "Deployment", "api", "d")
	converted["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["apiVersion"] = "apps/v1beta1"
	conflict := ownerObject("ReplicaSet", "conflict", "conflict", "ns", "Deployment", "api", "d")
	conflict["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["apiVersion"] = "example.com/v1"
	scope, _, err := ResolveScope(context.Background(), Options{Namespace: "ns", Workload: "deploy/api"}, graphKube(deployment, converted, conflict))
	if err != nil || !scope.Related["converted"] || scope.Related["conflict"] {
		t.Errorf("scope=%+v err=%v", scope, err)
	}
	for _, tc := range []struct {
		api  string
		want bool
	}{{"apps/v1beta1", true}, {"example.com/v1", false}} {
		matched, _ := scope.MatchObject(Object{APIVersion: tc.api, Kind: "Deployment", Namespace: "ns", Name: "api", UID: "d"})
		if matched != tc.want {
			t.Errorf("api=%s matched=%v", tc.api, matched)
		}
	}
	scope, gaps, err := ResolveScope(context.Background(), Options{Namespace: "ns", Workload: "rs/a"}, graphKube(
		ownerObject("ReplicaSet", "a", "a", "ns", "ReplicaSet", "b", "b"),
		ownerObject("ReplicaSet", "b", "b", "ns", "ReplicaSet", "a", "a"),
	))
	if err != nil || len(scope.Related) != 1 || !scope.Related["a"] {
		t.Fatalf("cycle scope=%+v err=%v", scope, err)
	}
	if len(gaps) == 0 || !strings.Contains(gaps[0].Message, "cycle") {
		t.Fatalf("cycle gaps=%+v", gaps)
	}
}

func TestExactOwnerGraphControllerAndFallbackAmbiguity(t *testing.T) {
	root := ownerObject("Deployment", "api", "d", "ns", "", "", "")
	noncontroller := ownerObject("Pod", "noncontroller", "noncontroller", "ns", "Deployment", "api", "d")
	noncontroller["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["controller"] = false
	wrongName := ownerObject("Pod", "wrong-name", "wrong-name", "ns", "Deployment", "other", "d")
	one := ownerObject("Pod", "recreated", "one", "ns", "Deployment", "api", "d")
	two := ownerObject("Pod", "recreated", "two", "ns", "Deployment", "api", "d")
	scope, _, err := ResolveScope(context.Background(), Options{Namespace: "ns", Workload: "deploy/api"}, graphKube(root, noncontroller, wrongName, one, two))
	if err != nil || scope.Related["noncontroller"] || scope.Related["wrong-name"] {
		t.Fatalf("scope=%+v err=%v", scope, err)
	}
	matched, gap := scope.MatchObject(Object{Kind: "Pod", Namespace: "ns", Name: "recreated"})
	if matched || gap != nil {
		t.Fatalf("ambiguous fallback=%v gap=%v", matched, gap)
	}
}
