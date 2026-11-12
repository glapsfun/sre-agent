package timeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
)

type gitOpsKube struct {
	lists   map[string]string
	runs    map[string]executil.Result
	failure executil.Result
}

func (k gitOpsKube) Context() string { return "pinned" }
func (k gitOpsKube) Run(_ context.Context, args ...string) executil.Result {
	if r, ok := k.runs[strings.Join(args, " ")]; ok {
		return r
	}
	return executil.Result{Err: fmt.Errorf("unexpected auxiliary request: %v", args)}
}
func (k gitOpsKube) Stream(_ context.Context, w io.Writer, args ...string) executil.Result {
	if len(args) != 5 || args[0] != "get" || strings.Join(args[2:], " ") != "--all-namespaces --chunk-size=500 --output=json" {
		return executil.Result{Err: fmt.Errorf("unexpected list request: %v", args)}
	}
	if body, ok := k.lists[args[1]]; ok {
		if _, err := io.WriteString(w, body); err != nil {
			return executil.Result{Err: err}
		}
	}
	return k.failure
}

func gitOpsScope() Scope {
	s := timelineScope()
	target := Object{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "ns", Name: "api", UID: "deploy-api"}
	s.Workload, s.Target = "deploy/api", &target
	s.Objects, s.Related = map[string]Object{target.UID: target}, map[string]bool{target.UID: true}
	return s
}

func fluxFixture(t *testing.T, key string) string {
	t.Helper()
	var data map[string][]any
	if err := json.Unmarshal([]byte(fixture(t, "flux")), &data); err != nil {
		t.Fatal(err)
	}
	return eventList(data[key]...)
}

func gitOpsJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMissingFluxAndArgoAreGaps(t *testing.T) {
	collectors := []struct {
		name string
		call func(context.Context, Scope, KubeRunner) SourceResult
	}{
		{"flux-kustomizations", CollectFluxKustomizations}, {"flux-helmreleases", CollectFluxHelmReleases}, {"argocd", CollectArgo},
	}
	for _, c := range collectors {
		for _, tc := range []struct {
			name, diagnostic, status string
			err                      error
			wantErrors               bool
		}{
			{"missing", "error: the server doesn't have a resource type", "missing", errors.New("exit status 1"), false},
			{"denied", "Error from server (Forbidden): password=fixture-hidden", "denied", errors.New("exit status 1"), true},
			{"timeout", "", "timeout", context.DeadlineExceeded, true},
			{"malformed", "", "error", nil, true},
		} {
			t.Run(c.name+"/"+tc.name, func(t *testing.T) {
				code := 1
				k := gitOpsKube{failure: executil.Result{Stderr: tc.diagnostic, Err: tc.err, ExitCode: &code}}
				r := c.call(context.Background(), gitOpsScope(), k)
				if r.Summary.Source != c.name || r.Summary.Status != tc.status || len(r.Entries) != 0 || !hasGap(r, tc.status) || (len(r.Errors) > 0) != tc.wantErrors {
					t.Fatalf("result=%+v", r)
				}
				if text := gitOpsJSON(t, r); strings.Contains(text, "fixture-hidden") || strings.Contains(text, "BEGIN EXTERNAL DATA") {
					t.Fatalf("unsafe diagnostic boundary: %s", text)
				}
			})
		}
	}
}

func TestFluxRevisionSemantics(t *testing.T) {
	k := gitOpsKube{lists: map[string]string{"kustomizations.kustomize.toolkit.fluxcd.io": fluxFixture(t, "kustomizations"), "helmreleases.helm.toolkit.fluxcd.io": fluxFixture(t, "helmReleases")}}
	r := CollectFluxKustomizations(context.Background(), gitOpsScope(), k)
	if len(r.Errors) != 0 || r.Summary.Matched != 2 || len(r.Entries) != 4 || !hasGap(r, "association") || !hasGap(r, "history") || !hasGap(r, "timestamp") {
		t.Fatalf("result=%+v", r)
	}
	var condition, success, failure bool
	for _, e := range r.Entries {
		if e.Object.UID != "k-api" && e.Object.UID != "k-fallback" {
			t.Fatalf("wrong manager: %+v", e)
		}
		switch e.TimeBasis {
		case "condition.lastTransitionTime":
			condition = true
			if e.Details["observed_generation"] != int64(7) || e.Details["type"] != "Ready" || e.Details["status"] != "False" {
				t.Fatalf("condition=%+v", e)
			}
			if _, ok := e.Details["revision"]; ok {
				t.Fatal("condition timestamp attached to revision")
			}
		case "history.lastReconciled":
			if e.Details["revision"] == "main@sha1:good" {
				success = true
				if e.Details["total_reconciliations"] != int64(2) || e.Details["status"] != "ReconciliationSucceeded" {
					t.Fatalf("history=%+v", e)
				}
			}
			if e.Details["revision"] == "main@sha1:attempt" {
				failure = true
				if e.Details["status"] != "HealthCheckFailed" || strings.Contains(e.Change, "successfully applied") {
					t.Fatalf("failed attempt promoted: %+v", e)
				}
			}
		default:
			t.Fatalf("invented time basis: %+v", e)
		}
	}
	if !condition || !success || !failure {
		t.Fatalf("lost status/history: %+v", r)
	}
	text := gitOpsJSON(t, r)
	if !strings.Contains(gitOpsJSON(t, r.Summary.Context), "main@sha1:undated") || strings.Contains(text, "fixture-hidden") || strings.Contains(text, "DO_NOT_LEAK") {
		t.Fatalf("context/projection: %s", text)
	}

	h := CollectFluxHelmReleases(context.Background(), gitOpsScope(), k)
	if h.Summary.Matched != 1 || len(h.Entries) != 3 || len(h.Errors) != 0 {
		t.Fatalf("helm=%+v", h)
	}
	for _, e := range h.Entries {
		if e.TimeBasis == "history.lastDeployed" && e.Details["version"] == int64(3) && e.Details["status"] != "failed" {
			t.Fatalf("attempt promoted: %+v", e)
		}
	}
	if strings.Contains(gitOpsJSON(t, h), "DO_NOT_LEAK") {
		t.Fatal("Helm values or configuration digest leaked")
	}
}

func TestFluxRevisionScopeAndIncompleteHistory(t *testing.T) {
	var data map[string][]map[string]any
	if err := json.Unmarshal([]byte(fixture(t, "flux")), &data); err != nil {
		t.Fatal(err)
	}
	obj := data["kustomizations"][0]
	status := obj["status"].(map[string]any)
	status["inventory"] = map[string]any{"entries": []any{map[string]any{"id": "ns_api_other.example_Deployment", "v": "v1"}}}
	k := gitOpsKube{lists: map[string]string{"kustomizations.kustomize.toolkit.fluxcd.io": eventList(obj)}}
	if r := CollectFluxKustomizations(context.Background(), gitOpsScope(), k); r.Summary.Matched != 0 {
		t.Fatalf("group mismatch broadened: %+v", r)
	}
	status["inventory"] = map[string]any{"entries": []any{map[string]any{"id": "ns_api_apps_Deployment", "v": "v1"}}}
	delete(status, "history")
	status["conditions"] = []any{map[string]any{"type": "Ready", "status": "True", "reason": "Succeeded", "message": "fine"}}
	k.lists["kustomizations.kustomize.toolkit.fluxcd.io"] = eventList(obj)
	r := CollectFluxKustomizations(context.Background(), gitOpsScope(), k)
	if r.Summary.Matched != 1 || len(r.Entries) != 0 || !hasGap(r, "timestamp") || !hasGap(r, "history") {
		t.Fatalf("undated history invented: %+v", r)
	}
	s := gitOpsScope()
	s.Target = nil
	if r := CollectFluxKustomizations(context.Background(), s, k); r.Summary.Matched != 0 || len(r.Entries) != 0 {
		t.Fatalf("unresolved workload broadened: %+v", r)
	}
	s = timelineScope()
	if r := CollectFluxKustomizations(context.Background(), s, k); r.Summary.Matched != 1 {
		t.Fatalf("cross-namespace manager lost: %+v", r)
	}
}

func argoKube(body string) gitOpsKube {
	return gitOpsKube{lists: map[string]string{"applications.argoproj.io": body}, runs: map[string]executil.Result{
		"config view --minify --output=jsonpath={.clusters[0].cluster.server}": {Stdout: "https://cluster.example"},
	}}
}

func TestArgoAssociationAndHistory(t *testing.T) {
	k := argoKube(fixture(t, "argo"))
	k.runs["get deployment.apps api --namespace=ns --output=json"] = executil.Result{Stdout: `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"ns","uid":"deploy-api"}}`}
	r := CollectArgo(context.Background(), gitOpsScope(), k)
	if r.Summary.Matched != 1 || len(r.Entries) != 2 || len(r.Errors) != 0 || !hasGap(r, "association") || !hasGap(r, "history") || !hasGap(r, "timestamp") {
		t.Fatalf("argo=%+v", r)
	}
	for _, e := range r.Entries {
		if e.Object.UID != "a-api" || e.TimeBasis != "history.deployedAt" {
			t.Fatalf("wrong association/time: %+v", e)
		}
	}
	if r.Entries[0].Time != time.Date(2026, 11, 11, 11, 10, 0, 0, time.UTC) || r.Entries[0].Details["deploy_started_at"] == nil || !strings.Contains(gitOpsJSON(t, r.Entries[1]), "chart-2.0") {
		t.Fatalf("history fields lost: %+v", r)
	}
	if strings.Contains(gitOpsJSON(t, r), "DO_NOT_LEAK") {
		t.Fatal("Argo source values leaked")
	}
	s := gitOpsScope()
	s.Target = nil
	if r := CollectArgo(context.Background(), s, argoKube(fixture(t, "argo"))); r.Summary.Matched != 0 || len(r.Entries) != 0 {
		t.Fatalf("unresolved workload broadened: %+v", r)
	}
}

func TestArgoAssociationTrackingInstallation(t *testing.T) {
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(fixture(t, "argo")), &list); err != nil {
		t.Fatal(err)
	}
	a := list.Items[0]
	a["status"].(map[string]any)["resources"] = []any{}
	var b map[string]any
	if err := json.Unmarshal([]byte(gitOpsJSON(t, a)), &b); err != nil {
		t.Fatal(err)
	}
	b["metadata"].(map[string]any)["namespace"] = "argo-b"
	b["metadata"].(map[string]any)["uid"] = "a-other-installation"
	b["status"].(map[string]any)["controllerNamespace"] = "argo-b"
	k := argoKube(eventList(a, b))
	target := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"namespace": "ns", "name": "api", "uid": "deploy-api", "annotations": map[string]any{"argocd.argoproj.io/tracking-id": "api:apps/Deployment:ns/api", "argocd.argoproj.io/installation-id": "installation-a"}}}
	k.runs["get deployment.apps api --namespace=ns --output=json"] = executil.Result{Stdout: gitOpsJSON(t, target)}
	k.runs["get configmap argocd-cm --namespace=argo-a --output=jsonpath={.data.installationID}"] = executil.Result{Stdout: "installation-a"}
	k.runs["get configmap argocd-cm --namespace=argo-b --output=jsonpath={.data.installationID}"] = executil.Result{Stdout: "installation-b"}
	r := CollectArgo(context.Background(), gitOpsScope(), k)
	if r.Summary.Matched != 1 || len(r.Entries) != 2 || len(r.Errors) != 0 {
		t.Fatalf("installation association=%+v", r)
	}
	for _, e := range r.Entries {
		if e.Object.UID != "a-api" {
			t.Fatalf("wrong installation: %+v", e)
		}
	}
	for _, mutate := range []func(){
		func() {
			target["metadata"].(map[string]any)["annotations"].(map[string]any)["argocd.argoproj.io/tracking-id"] = "api:apps/Deployment:other/api"
		},
		func() { target["metadata"].(map[string]any)["uid"] = "recreated" },
	} {
		target["metadata"].(map[string]any)["annotations"].(map[string]any)["argocd.argoproj.io/tracking-id"] = "api:apps/Deployment:ns/api"
		target["metadata"].(map[string]any)["uid"] = "deploy-api"
		mutate()
		k.runs["get deployment.apps api --namespace=ns --output=json"] = executil.Result{Stdout: gitOpsJSON(t, target)}
		if r := CollectArgo(context.Background(), gitOpsScope(), k); r.Summary.Matched != 0 || !hasGap(r, "association") {
			t.Fatalf("copied/recreated tracking accepted: %+v", r)
		}
	}
}

func argoObjects(t *testing.T) []map[string]any {
	t.Helper()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(fixture(t, "argo")), &list); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestArgoAssociationDistinctDescendantsAndConflicts(t *testing.T) {
	objects := argoObjects(t)
	a, b := objects[0], objects[1]
	b["status"].(map[string]any)["resources"] = []any{map[string]any{"group": "", "kind": "Pod", "namespace": "ns", "name": "api-pod"}}
	s := gitOpsScope()
	s.Objects["pod-api"] = Object{APIVersion: "v1", Kind: "Pod", Namespace: "ns", Name: "api-pod", UID: "pod-api"}
	s.Related["pod-api"] = true
	k := argoKube(eventList(a, b))
	r := CollectArgo(context.Background(), s, k)
	if r.Summary.Matched != 2 || len(r.Entries) != 3 || len(r.Errors) != 0 {
		t.Fatalf("independent descendant memberships were collapsed: %+v", r)
	}
	b["status"].(map[string]any)["resources"] = a["status"].(map[string]any)["resources"]
	k.lists["applications.argoproj.io"] = eventList(a, b)
	k.runs["get deployment.apps api --namespace=ns --output=json"] = executil.Result{Stdout: `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"ns","uid":"deploy-api"}}`}
	if r := CollectArgo(context.Background(), s, k); r.Summary.Matched != 0 || !hasGap(r, "association") {
		t.Fatalf("conflicting claims accepted: %+v", r)
	}
	if r := CollectArgo(context.Background(), timelineScope(), k); r.Summary.Matched != 2 || !hasGap(r, "association") {
		t.Fatalf("namespace evidence or conflict gap lost: %+v", r)
	}
}

func TestArgoAssociationTrackingAlongsideIndependentDescendant(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resources []any
	}{
		{"empty", []any{}},
		{"unrelated", []any{map[string]any{"group": "", "kind": "ConfigMap", "namespace": "ns", "name": "settings"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := argoObjects(t)
			a, b := objects[0], objects[1]
			a["status"].(map[string]any)["resources"] = tc.resources
			b["status"].(map[string]any)["resources"] = []any{map[string]any{"group": "", "kind": "Pod", "namespace": "ns", "name": "api-pod"}}
			s := gitOpsScope()
			s.Objects["pod-api"] = Object{APIVersion: "v1", Kind: "Pod", Namespace: "ns", Name: "api-pod", UID: "pod-api"}
			s.Related["pod-api"] = true
			k := argoKube(eventList(a, b))
			k.runs["get deployment.apps api --namespace=ns --output=json"] = executil.Result{Stdout: `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"ns","uid":"deploy-api","annotations":{"argocd.argoproj.io/tracking-id":"api:apps/Deployment:ns/api","argocd.argoproj.io/installation-id":"installation-a"}}}`}
			k.runs["get configmap argocd-cm --namespace=argo-a --output=jsonpath={.data.installationID}"] = executil.Result{Stdout: "installation-a"}
			r := CollectArgo(context.Background(), s, k)
			if r.Summary.Matched != 2 || len(r.Entries) != 3 || len(r.Errors) != 0 {
				t.Fatalf("tracking history suppressed by independent descendant membership: %+v", r)
			}
			k.runs["get configmap argocd-cm --namespace=argo-a --output=jsonpath={.data.installationID}"] = executil.Result{Err: errors.New("installation unavailable")}
			r = CollectArgo(context.Background(), s, k)
			if r.Summary.Matched != 1 || len(r.Entries) != 1 || r.Entries[0].Object.UID != "a-gateway" || len(r.Errors) != 1 {
				t.Fatalf("failed installation lookup erased independent descendant history: %+v", r)
			}
			k.runs["get deployment.apps api --namespace=ns --output=json"] = executil.Result{Err: errors.New("metadata unavailable")}
			r = CollectArgo(context.Background(), s, k)
			if r.Summary.Matched != 1 || len(r.Entries) != 1 || r.Entries[0].Object.UID != "a-gateway" || len(r.Errors) != 1 {
				t.Fatalf("failed tracking lookup erased independent descendant history: %+v", r)
			}
			a["spec"].(map[string]any)["destination"].(map[string]any)["server"] = "https://remote.example"
			k.lists["applications.argoproj.io"] = eventList(a, b)
			r = CollectArgo(context.Background(), s, k)
			if r.Summary.Matched != 1 || len(r.Entries) != 1 || r.Entries[0].Object.UID != "a-gateway" || len(r.Errors) != 0 || !hasGap(r, "association") {
				t.Fatalf("remote candidate triggered tracking or erased independent descendant history: %+v", r)
			}
		})
	}
}

func TestArgoAssociationRemoteOnlyDoesNotReadWorkload(t *testing.T) {
	app := argoObjects(t)[2]
	r := CollectArgo(context.Background(), gitOpsScope(), argoKube(eventList(app)))
	if r.Summary.Matched != 0 || len(r.Errors) != 0 || !hasGap(r, "association") {
		t.Fatalf("remote-only source performed unnecessary tracking lookup: %+v", r)
	}
}

func TestArgoAssociationAuxiliaryFailuresPreserveEvidence(t *testing.T) {
	app := argoObjects(t)[0]
	app["spec"].(map[string]any)["destination"].(map[string]any)["server"] = "https://kubernetes.default.svc"
	k := argoKube(eventList(app))
	k.runs["config view --minify --output=jsonpath={.clusters[0].cluster.server}"] = executil.Result{Err: errors.New("config unavailable"), Stderr: "password=fixture-hidden"}
	r := CollectArgo(context.Background(), gitOpsScope(), k)
	if r.Summary.Matched != 1 || len(r.Entries) != 2 || len(r.Errors) != 1 || !hasGap(r, "error") || strings.Contains(gitOpsJSON(t, r), "fixture-hidden") {
		t.Fatalf("independent in-cluster evidence lost: %+v", r)
	}
}

func TestArgoAssociationInstallationExactBytesAndUnknownController(t *testing.T) {
	app := argoObjects(t)[0]
	app["status"].(map[string]any)["resources"] = []any{}
	k := argoKube(eventList(app))
	k.runs["get deployment.apps api --namespace=ns --output=json"] = executil.Result{Stdout: `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"ns","uid":"deploy-api","annotations":{"argocd.argoproj.io/tracking-id":"api:apps/Deployment:ns/api","argocd.argoproj.io/installation-id":"installation-a"}}}`}
	k.runs["get configmap argocd-cm --namespace=argo-a --output=jsonpath={.data.installationID}"] = executil.Result{Stdout: " installation-a "}
	if r := CollectArgo(context.Background(), gitOpsScope(), k); r.Summary.Matched != 0 || !hasGap(r, "association") {
		t.Fatalf("installation bytes changed during comparison: %+v", r)
	}
	delete(app["status"].(map[string]any), "controllerNamespace")
	k.lists["applications.argoproj.io"] = eventList(app)
	if r := CollectArgo(context.Background(), gitOpsScope(), k); r.Summary.Matched != 0 || !hasGap(r, "association") {
		t.Fatalf("unknown controller assumed: %+v", r)
	}
}

func TestArgoHistoryInvalidRecordsAndAbsentHistory(t *testing.T) {
	app := argoObjects(t)[0]
	app["status"].(map[string]any)["history"] = []any{
		map[string]any{"id": -1, "revision": "bad-id", "deployedAt": "2026-11-11T11:00:00Z"},
		map[string]any{"id": 2, "deployedAt": "2026-11-11T11:00:00Z"},
		map[string]any{"id": 3, "revisions": []string{"good", ""}, "deployedAt": "2026-11-11T11:00:00Z"},
		map[string]any{"id": 4, "revision": "good", "deployedAt": "2026-11-11T11:00:00Z"},
	}
	r := CollectArgo(context.Background(), gitOpsScope(), argoKube(eventList(app)))
	if len(r.Entries) != 1 || r.Entries[0].Details["revision"] != "good" || !hasGap(r, "error") {
		t.Fatalf("invalid history promoted: %+v", r)
	}
	delete(app["status"].(map[string]any), "history")
	if r := CollectArgo(context.Background(), gitOpsScope(), argoKube(eventList(app))); len(r.Entries) != 0 || r.Summary.Matched != 1 || !hasGap(r, "history") {
		t.Fatalf("absent history not explicit: %+v", r)
	}
}

func TestArgoAssociationRejectsLabelsAndUnknownDestination(t *testing.T) {
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(fixture(t, "argo")), &list); err != nil {
		t.Fatal(err)
	}
	a := list.Items[0]
	a["status"].(map[string]any)["resources"] = []any{}
	k := argoKube(eventList(a))
	k.runs["get deployment.apps api --namespace=ns --output=json"] = executil.Result{Stdout: `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"ns","uid":"deploy-api","labels":{"app.kubernetes.io/instance":"api"}}}`}
	r := CollectArgo(context.Background(), gitOpsScope(), k)
	if r.Summary.Matched != 0 || !hasGap(r, "association") {
		t.Fatalf("generic label accepted: %+v", r)
	}
}

func TestGitOpsFailedListsDiscardPartialEvidence(t *testing.T) {
	code := 7
	k := argoKube(fixture(t, "argo"))
	k.failure = executil.Result{Err: errors.New("exit status 7"), ExitCode: &code, Stderr: "transport failed"}
	if r := CollectArgo(context.Background(), gitOpsScope(), k); len(r.Entries) != 0 || r.Summary.Matched != 0 || len(r.Errors) == 0 {
		t.Fatalf("partial Argo retained: %+v", r)
	}
	k = gitOpsKube{lists: map[string]string{"kustomizations.kustomize.toolkit.fluxcd.io": fluxFixture(t, "kustomizations")}, failure: executil.Result{Err: errors.New("exit status 7"), ExitCode: &code}}
	if r := CollectFluxKustomizations(context.Background(), gitOpsScope(), k); len(r.Entries) != 0 || r.Summary.Matched != 0 || len(r.Errors) == 0 {
		t.Fatalf("partial Flux retained: %+v", r)
	}
}
