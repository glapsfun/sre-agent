package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
)

type fixtureRunner struct {
	mu       sync.Mutex
	calls    []string
	fixtures map[string]executil.Result
	hang     string
}

func (f *fixtureRunner) Run(ctx context.Context, name string, args ...string) executil.Result {
	key := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if name == f.hang {
		<-ctx.Done()
		return executil.Result{Err: ctx.Err()}
	}
	if r, ok := f.fixtures[key]; ok {
		return r
	}
	return executil.Result{Err: fmt.Errorf("unexpected command %s", key)}
}
func fixtureService(f *fixtureRunner, missing ...string) Service {
	return Service{Runner: f, LookPath: func(name string) (string, error) {
		for _, m := range missing {
			if m == name {
				return "", errors.New("missing")
			}
		}
		return "/bin/" + name, nil
	}}
}
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SRE_SKIP_CLUSTER", "SRE_KUBE_CONTEXT", "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION"} {
		t.Setenv(k, "")
	}
}
func jsonResult(s string) executil.Result { return executil.Result{Stdout: s} }
func cloudFixtures() map[string]executil.Result {
	return map[string]executil.Result{
		"aws sts get-caller-identity --output json --no-cli-pager --no-cli-auto-prompt": jsonResult(`{"Account":"123456789012","Arn":"arn:aws:iam::123456789012:role/test"}`),
		"aws configure get region":                                      jsonResult("eu-west-1\n"),
		"gcloud auth list --filter=status:ACTIVE --format=json --quiet": jsonResult(`[{"account":"test@example.com","status":"ACTIVE"}]`),
		"gcloud config list --format=json --quiet":                      jsonResult(`{"core":{"project":"my-proj"},"compute":{"region":"europe-west1","zone":"europe-west1-b"}}`),
		"gcloud projects list --limit=20 --format=json --quiet":         jsonResult(`[{"projectId":"my-proj","name":"My project","lifecycleState":"ACTIVE"}]`),
		"gh repo view --json nameWithOwner,url":                         jsonResult(`{"nameWithOwner":"owner/repo","url":"https://github.com/owner/repo"}`),
	}
}
func TestParallelTimeoutPreservesEveryOtherResult(t *testing.T) {
	cleanEnv(t)
	f := &fixtureRunner{fixtures: cloudFixtures(), hang: "aws"}
	start := time.Now()
	report, err := fixtureService(f, "kubectl", "az", "k6").Discover(context.Background(), Options{Timeout: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("hanging probe delayed peers")
	}
	if len(report.Probes) != 5 || report.Probes["aws"].Status != Timeout {
		t.Fatalf("statuses: %+v", report)
	}
	if report.Probes["gcloud"].Status != OK || report.Probes["github"].Status != OK || report.Probes["obs"].Status != Missing {
		t.Fatalf("lost peer results: %+v", report)
	}
	if report.Probes["github"].Findings.GitHub.NameWithOwner != "owner/repo" {
		t.Fatal("lost GitHub finding")
	}
}
func TestCloudFactsAndPartialFailure(t *testing.T) {
	cleanEnv(t)
	t.Setenv("AWS_PROFILE", "production")
	fixtures := cloudFixtures()
	fixtures["gcloud projects list --limit=20 --format=json --quiet"] = executil.Result{Err: errors.New("exit 1"), Stderr: "PERMISSION_DENIED"}
	f := &fixtureRunner{fixtures: fixtures}
	report, err := fixtureService(f).Discover(context.Background(), Options{Only: []string{"aws", "gcloud", "github", "github"}})
	if err != nil {
		t.Fatal(err)
	}
	aws := report.Probes["aws"].Findings.AWS
	if aws == nil || aws.Account != "123456789012" || aws.ARN != "arn:aws:iam::123456789012:role/test" || aws.Profile != "production" || aws.Region != "eu-west-1" {
		t.Fatalf("AWS facts: %+v", aws)
	}
	gc := report.Probes["gcloud"]
	if gc.Status != Denied || gc.Findings.GCloud == nil || gc.Findings.GCloud.Project != "my-proj" || gc.Findings.GCloud.Account != "test@example.com" || gc.Findings.GCloud.Zone != "europe-west1-b" {
		t.Fatalf("GCP partial results: %+v", gc)
	}
	if len(report.Probes) != 3 {
		t.Fatal("duplicates not deduplicated")
	}
}
func clusterFixtures() map[string]executil.Result {
	return map[string]executil.Result{
		"kubectl config current-context":                                    jsonResult("original\n"),
		"kubectl --context original version -o json":                        jsonResult(`{"clientVersion":{"gitVersion":"v1.34.0"},"serverVersion":{"gitVersion":"v1.33.0"}}`),
		"kubectl --context original get namespaces -o json":                 jsonResult(`{"items":[{"metadata":{"name":"monitoring"}},{"metadata":{"name":"istio-system"}}]}`),
		"kubectl --context original get nodes -o json":                      jsonResult(`{"items":[{"metadata":{"name":"worker","labels":{"cloud.google.com/gke-nodepool":"pool"}},"spec":{"providerID":"gce://my-proj/zone/worker"}}]}`),
		"kubectl --context original get daemonsets -n kube-system -o json":  jsonResult(`{"items":[]}`),
		"kubectl --context original get crds -o json":                       jsonResult(`{"items":[{"metadata":{"name":"kustomizations.kustomize.toolkit.fluxcd.io"}},{"metadata":{"name":"applications.argoproj.io"}},{"metadata":{"name":"providers.pkg.crossplane.io"}},{"metadata":{"name":"serviceprofiles.linkerd.io"}},{"metadata":{"name":"testruns.k6.io"}}]}`),
		"kubectl --context original get services --all-namespaces -o json":  jsonResult(`{"items":[{"metadata":{"namespace":"monitoring","name":"grafana"},"spec":{"ports":[{"port":80},{"port":443}]}},{"metadata":{"namespace":"monitoring","name":"jaeger-collector"},"spec":{"ports":[{"port":14250}]}},{"metadata":{"namespace":"monitoring","name":"jaeger-query"},"spec":{"ports":[{"port":8080}]}}]}`),
		"kubectl --context original get ingresses --all-namespaces -o json": jsonResult(`{"items":[{"metadata":{"namespace":"monitoring","name":"dashboard"},"spec":{"rules":[{"host":"grafana.example.com"}]}},{"metadata":{"namespace":"app","name":"checkout"},"spec":{"rules":[{"host":"checkout.example.com"}]}}]}`),
		"k6 version":              jsonResult("k6 v1.0.0\n"),
		"az account show -o json": jsonResult(`{"name":"Production"}`),
	}
}
func TestClusterParityAndOnePinnedContext(t *testing.T) {
	cleanEnv(t)
	fixtures := clusterFixtures()
	// The GKE signal occurs after 400KB of unrelated metadata.
	fixtures["kubectl --context original get nodes -o json"] = jsonResult(`{"items":[{"metadata":{"annotations":{"padding":"` + strings.Repeat("x", 450000) + `"}},"spec":{"providerID":"gce://my-proj/zone/first"}},{"metadata":{"labels":{"cloud.google.com/gke-nodepool":"pool"}},"spec":{"providerID":"gce://my-proj/zone/last"}}]}`)
	f := &fixtureRunner{fixtures: fixtures}
	report, err := fixtureService(f).Discover(context.Background(), Options{Only: []string{"env", "obs"}})
	if err != nil {
		t.Fatal(err)
	}
	env := report.Probes["env"]
	if env.Status != OK || env.Findings.Env == nil {
		t.Fatalf("env: %+v", env)
	}
	e := env.Findings.Env
	if e.Context != "original" || e.GKE == nil || !*e.GKE || e.EKS == nil || *e.EKS || e.Flux == nil || !*e.Flux || e.ArgoCD == nil || !*e.ArgoCD || e.Crossplane == nil || !*e.Crossplane || e.ServerVersion != "v1.33.0" || e.AzureSubscription != "Production" || len(e.Tools) != 16 {
		t.Fatalf("environment parity: %+v", e)
	}
	obs := report.Probes["obs"]
	if obs.Status != OK || obs.Findings.Obs == nil {
		t.Fatalf("obs: %+v", obs)
	}
	o := obs.Findings.Obs
	if len(o.Endpoints) != 2 || o.Istio == nil || !*o.Istio || o.Linkerd == nil || !*o.Linkerd || !o.K6CLI || o.K6Operator == nil || !*o.K6Operator || o.K6Version != "k6 v1.0.0" || len(o.Ingresses) != 1 {
		t.Fatalf("observability parity: %+v", o)
	}
	ep := o.Endpoints[0]
	if ep.Component != "grafana" || strings.Join(ep.PortForward, " ") != "kubectl --context original port-forward -n monitoring svc/grafana 3000:80" || ep.ReadinessURL != "http://localhost:3000/api/health" {
		t.Fatalf("endpoint: %+v", ep)
	}
	count := 0
	for _, call := range f.calls {
		if call == "kubectl config current-context" {
			count++
		} else if strings.HasPrefix(call, "kubectl ") && !strings.HasPrefix(call, "kubectl --context original ") {
			t.Fatalf("unpinned command: %s", call)
		}
	}
	if count != 1 {
		t.Fatalf("context resolved %d times", count)
	}
}
func TestDeniedAndMalformedDoNotBecomeAbsence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response executil.Result
		status   Status
	}{
		{"denied", executil.Result{Err: errors.New("exit 1"), Stderr: "Forbidden"}, Denied},
		{"malformed", jsonResult(`{"items":`), Error},
		{"null", jsonResult(`null`), Error},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanEnv(t)
			fixtures := clusterFixtures()
			fixtures["kubectl --context original get crds -o json"] = tc.response
			report, err := fixtureService(&fixtureRunner{fixtures: fixtures}, "az").Discover(context.Background(), Options{Only: []string{"env"}, Context: "original"})
			if err != nil {
				t.Fatal(err)
			}
			p := report.Probes["env"]
			if p.Status != tc.status || p.Findings.Env == nil || p.Findings.Env.Flux != nil || len(p.Gaps) == 0 {
				t.Fatalf("failure implied absence: %+v", p)
			}
		})
	}
}
func TestRedactionAndStructuredBoundaries(t *testing.T) {
	cleanEnv(t)
	f := &fixtureRunner{fixtures: map[string]executil.Result{"gh repo view --json nameWithOwner,url": jsonResult(`{"nameWithOwner":"owner/repo token=secret-value","url":"https://user:pass@github.com/owner/repo"}`)}}
	r, err := fixtureService(f).Discover(context.Background(), Options{Only: []string{"github"}})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), "secret-value") || strings.Contains(string(data), "user:pass") || !strings.Contains(string(data), "[REDACTED]") {
		t.Fatalf("redaction: %s", data)
	}
	p := r.Probes["github"]
	if !strings.Contains(p.Begin, "BEGIN EXTERNAL DATA") || !strings.Contains(p.End, "END EXTERNAL DATA") || p.Findings.GitHub == nil {
		t.Fatalf("missing boundary: %+v", p)
	}
}

type delayedRunner struct {
	base  Runner
	delay time.Duration
}

func (d delayedRunner) Run(ctx context.Context, name string, args ...string) executil.Result {
	select {
	case <-ctx.Done():
		return executil.Result{Err: ctx.Err()}
	case <-time.After(d.delay):
		return d.base.Run(ctx, name, args...)
	}
}
func TestOverallProbeBudgetRetainsEarlierFacts(t *testing.T) {
	cleanEnv(t)
	f := &fixtureRunner{fixtures: cloudFixtures()}
	s := fixtureService(f)
	s.Runner = delayedRunner{base: f, delay: 30 * time.Millisecond}
	start := time.Now()
	r, err := s.Discover(context.Background(), Options{Only: []string{"aws"}, Timeout: 45 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Fatal("sequential calls multiplied timeout")
	}
	p := r.Probes["aws"]
	if p.Status != Timeout || p.Findings.AWS == nil || p.Findings.AWS.Account != "123456789012" || p.Findings.AWS.Region != "" {
		t.Fatalf("partial timeout findings: %+v", p)
	}
}
func TestCompactProviderSampleStillScansAllNodes(t *testing.T) {
	cleanEnv(t)
	fixtures := clusterFixtures()
	nodes := []string{}
	for i := 0; i < 5000; i++ {
		label := ""
		if i == 4999 {
			label = `"labels":{"cloud.google.com/gke-nodepool":"pool"}`
		}
		nodes = append(nodes, fmt.Sprintf(`{"metadata":{%s},"spec":{"providerID":"gce://my-proj/zone/worker-%d"}}`, label, i))
	}
	fixtures["kubectl --context original get nodes -o json"] = jsonResult(`{"items":[` + strings.Join(nodes, ",") + `]}`)
	r, err := fixtureService(&fixtureRunner{fixtures: fixtures}).Discover(context.Background(), Options{Only: []string{"env"}, Context: "original"})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Probes["env"]
	data, _ := json.Marshal(r)
	if p.Findings.Env == nil || p.Findings.Env.GKE == nil || !*p.Findings.Env.GKE {
		t.Fatal("missed late GKE label")
	}
	if len(data) > 10000 || len(p.Findings.Env.ProviderIDs) > 20 || len(p.Gaps) == 0 {
		t.Fatalf("unbounded provider report: %d bytes, gaps=%v", len(data), p.Gaps)
	}
}

func TestBoundedInventoriesAndK6FirstLine(t *testing.T) {
	cleanEnv(t)
	fixtures := clusterFixtures()
	services, ingresses, namespaces := []string{}, []string{}, []string{}
	for i := 0; i < 250; i++ {
		services = append(services, fmt.Sprintf(`{"metadata":{"namespace":"monitoring","name":"prometheus-%d"},"spec":{"ports":[{"port":9090}]}}`, i))
		ingresses = append(ingresses, fmt.Sprintf(`{"metadata":{"namespace":"monitoring","name":"prometheus-%d"},"spec":{"rules":[{"host":"metrics.example.com"}]}}`, i))
		namespaces = append(namespaces, fmt.Sprintf(`{"metadata":{"name":"ns-%d"}}`, i))
	}
	fixtures["kubectl --context original get services --all-namespaces -o json"] = jsonResult(`{"items":[` + strings.Join(services, ",") + `]}`)
	fixtures["kubectl --context original get ingresses --all-namespaces -o json"] = jsonResult(`{"items":[` + strings.Join(ingresses, ",") + `]}`)
	fixtures["kubectl --context original get namespaces -o json"] = jsonResult(`{"items":[` + strings.Join(namespaces, ",") + `]}`)
	fixtures["k6 version"] = jsonResult("k6 v1.0.0\nextra output\n")
	r, err := fixtureService(&fixtureRunner{fixtures: fixtures}).Discover(context.Background(), Options{Only: []string{"env", "obs"}, Context: "original"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r)
	if len(data) > 24000 {
		t.Fatalf("report too large: %d bytes", len(data))
	}
	if len(r.Probes["env"].Gaps) == 0 || len(r.Probes["obs"].Gaps) < 2 {
		t.Fatal("omissions not disclosed")
	}
	if r.Probes["obs"].Findings.Obs.K6Version != "k6 v1.0.0" {
		t.Fatal("k6 version included unrelated output")
	}
}

func TestAWSConfigFailureIsNotMissingRegion(t *testing.T) {
	cleanEnv(t)
	fixtures := cloudFixtures()
	fixtures["aws configure get region"] = executil.Result{Err: errors.New("unexpected execution failure")}
	r, err := fixtureService(&fixtureRunner{fixtures: fixtures}).Discover(context.Background(), Options{Only: []string{"aws"}})
	if err != nil {
		t.Fatal(err)
	}
	if p := r.Probes["aws"]; p.Status != Error || p.Findings.AWS.Account != "123456789012" {
		t.Fatalf("configuration failure hidden: %+v", p)
	}
}

func TestEKSRequiresAWSProviderAndCNI(t *testing.T) {
	for _, tc := range []struct {
		name, id, daemonsets string
		want                 bool
	}{
		{"EKS", "aws:///eu-west-1a/i-123", `{"items":[{"metadata":{"name":"aws-node"}}]}`, true},
		{"CNI without AWS", "gce://my-proj/zone/worker", `{"items":[{"metadata":{"name":"aws-node"}}]}`, false},
		{"AWS without CNI", "aws:///eu-west-1a/i-123", `{"items":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanEnv(t)
			fixtures := clusterFixtures()
			fixtures["kubectl --context original get nodes -o json"] = jsonResult(`{"items":[{"metadata":{},"spec":{"providerID":"` + tc.id + `"}}]}`)
			fixtures["kubectl --context original get daemonsets -n kube-system -o json"] = jsonResult(tc.daemonsets)
			r, err := fixtureService(&fixtureRunner{fixtures: fixtures}).Discover(context.Background(), Options{Only: []string{"env"}, Context: "original"})
			if err != nil {
				t.Fatal(err)
			}
			e := r.Probes["env"].Findings.Env
			if e.EKS == nil || *e.EKS != tc.want {
				t.Fatalf("EKS detection: %+v", e)
			}
		})
	}
}

func TestAllObservabilityComponentsAndGCPProjects(t *testing.T) {
	cleanEnv(t)
	fixtures := clusterFixtures()
	for key, value := range cloudFixtures() {
		fixtures[key] = value
	}
	services := []string{}
	for _, name := range []string{"prometheus", "alertmanager", "grafana", "loki", "mimir", "tempo", "jaeger", "elasticsearch", "opensearch"} {
		services = append(services, `{"metadata":{"name":"`+name+`","namespace":"monitoring"},"spec":{"ports":[{"port":8080}]}}`)
	}
	fixtures["kubectl --context original get services --all-namespaces -o json"] = jsonResult(`{"items":[` + strings.Join(services, ",") + `]}`)
	r, err := fixtureService(&fixtureRunner{fixtures: fixtures}).Discover(context.Background(), Options{Only: []string{"obs", "gcloud"}, Context: "original"})
	if err != nil {
		t.Fatal(err)
	}
	endpoints := r.Probes["obs"].Findings.Obs.Endpoints
	if len(endpoints) != 9 {
		t.Fatalf("missing endpoint components: %+v", endpoints)
	}
	g := r.Probes["gcloud"].Findings.GCloud
	if g.Region != "europe-west1" || len(g.Projects) != 1 || g.Projects[0].ID != "my-proj" || g.Projects[0].Name != "My project" || g.Projects[0].State != "ACTIVE" {
		t.Fatalf("GCP parity: %+v", g)
	}
}
