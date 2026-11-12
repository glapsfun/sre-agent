package timeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func alertScope(t *testing.T) Scope {
	t.Helper()
	scope, _, err := ResolveScope(context.Background(), Options{Namespace: "ns", Workload: "deploy/api"}, listKube(fixture(t, "owners")))
	if err != nil {
		t.Fatal(err)
	}
	scope.Start = timelineScope().Start
	scope.End = timelineScope().End
	return scope
}
func gapCount(r SourceResult, code string) int {
	count := 0
	for _, g := range r.Gaps {
		if g.Code == code {
			count += g.Count
		}
	}
	return count
}

func TestFiringAlertsAndExactScope(t *testing.T) {
	body := fixture(t, "alerts")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
	defer server.Close()
	scope := alertScope(t)
	scope.AlertmanagerURL = server.URL
	r := CollectAlerts(context.Background(), scope, server.Client())
	if len(r.Errors) != 0 || len(r.Entries) != 3 || r.Summary.Observed != 12 || r.Summary.Matched != 8 || gapCount(r, "association") != 3 || gapCount(r, "timestamp") != 2 || !hasGap(r, "history") {
		t.Fatalf("result=%+v", r)
	}
	if r.Summary.Context["firing_before_window"] != 1 {
		t.Fatalf("context=%+v", r.Summary.Context)
	}
	for i, id := range []string{"active", "suppressed", "pending"} {
		if r.Entries[i].ID != id || r.Entries[i].TimeBasis != "startsAt" || r.Entries[i].Time.Format(time.RFC3339) != "2026-11-11T11:00:00Z" {
			t.Fatalf("entry=%+v", r.Entries[i])
		}
	}
	if r.Entries[1].Details["state"] != "suppressed" || r.Entries[1].Details["silenced"] != true || r.Entries[1].Details["inhibited"] != true {
		t.Fatalf("suppressed=%+v", r.Entries[1])
	}
	encoded, _ := json.Marshal(r)
	for _, secret := range []string{"label-hidden", "summary-hidden", "description-hidden", "annotation-hidden", "generator-hidden", "date-hidden", "silence-id", "inhibitor-id", "BEGIN EXTERNAL"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("retained %s", secret)
		}
	}
	scope.Target = nil
	r = CollectAlerts(context.Background(), scope, server.Client())
	if len(r.Entries) != 0 || !hasGap(r, "association") {
		t.Fatalf("unresolved scope=%+v", r)
	}
	scope.Workload = ""
	r = CollectAlerts(context.Background(), scope, server.Client())
	if len(r.Entries) != 6 {
		t.Fatalf("namespace result=%+v", r)
	}
}

type alertDoerFunc func(*http.Request) (*http.Response, error)

func (f alertDoerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestFiringAlertsLabelConventions(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		labels               map[string]string
		entries, association int
	}{
		{"scrape job with pod", map[string]string{"namespace": "ns", "pod": "api-pod", "job": "kube-state-metrics"}, 1, 0},
		{"scrape job only", map[string]string{"namespace": "ns", "job": "api"}, 0, 1},
		{"namespace aliases conflict", map[string]string{"namespace": "ns", "kubernetes_namespace": "other", "pod": "api-pod"}, 0, 1},
		{"pod aliases conflict", map[string]string{"namespace": "ns", "pod": "api-pod", "kubernetes_pod_name": "other"}, 0, 1},
		{"namespace and pod aliases", map[string]string{"kubernetes_namespace": "ns", "kubernetes_pod_name": "api-pod"}, 1, 0},
		{"other namespace", map[string]string{"namespace": "other", "pod": "api-pod"}, 0, 0},
		{"unknown workload kind", map[string]string{"namespace": "ns", "workload": "api", "workload_type": "unknown"}, 0, 1},
		{"type without name", map[string]string{"namespace": "ns", "workload_type": "deployment"}, 0, 1},
		{"ambiguous untyped name", map[string]string{"namespace": "ns", "workload": "api"}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := alertScope(t)
			s.AlertmanagerURL = "https://alerts.example.invalid"
			if tc.name == "ambiguous untyped name" {
				s.Objects["other"] = Object{Kind: "StatefulSet", Namespace: "ns", Name: "api", UID: "other"}
			}
			body, _ := json.Marshal([]any{map[string]any{"fingerprint": "id", "startsAt": "2026-11-11T10:00:00Z", "endsAt": "2026-11-11T13:00:00Z", "labels": tc.labels}})
			r := CollectAlerts(context.Background(), s, alertDoerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			}))
			if len(r.Errors) != 0 || len(r.Entries) != tc.entries || gapCount(r, "association") != tc.association {
				t.Fatalf("result=%+v", r)
			}
			if tc.name == "scrape job with pod" && r.Entries[0].Details["labels"].(map[string]any)["job"] != "kube-state-metrics" {
				t.Fatalf("labels=%+v", r.Entries[0].Details)
			}
		})
	}
}

func TestAlertTransportDeadlineAndBatch(t *testing.T) {
	for _, duration := range []time.Duration{0, time.Minute} {
		t.Run(duration.String(), func(t *testing.T) {
			ctx := context.Background()
			var want time.Time
			if duration != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, duration)
				defer cancel()
				want, _ = ctx.Deadline()
			}
			s := timelineScope()
			s.AlertmanagerURL = "https://alerts.example.invalid"
			r := CollectAlerts(ctx, s, alertDoerFunc(func(request *http.Request) (*http.Response, error) {
				got, ok := request.Context().Deadline()
				if !ok {
					t.Error("missing request deadline")
				}
				if !want.IsZero() && !want.Equal(got) {
					t.Errorf("deadline shortened: got %s want %s", got, want)
				}
				if want.IsZero() && (time.Until(got) > 30*time.Second || time.Until(got) < 29*time.Second) {
					t.Errorf("fallback deadline=%s", got)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]"))}, nil
			}))
			if len(r.Errors) != 0 {
				t.Fatalf("result=%+v", r)
			}
		})
	}
	for _, tc := range []struct {
		name, body string
		failed     bool
	}{
		{"response above eight MiB", strings.Repeat(" ", 9<<20) + "[]", false},
		{"partial batch", strings.TrimSuffix(fixture(t, "alerts"), "]\n") + ",", true},
		{"second JSON value", "[][]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := alertScope(t)
			s.AlertmanagerURL = "https://alerts.example.invalid"
			r := CollectAlerts(context.Background(), s, alertDoerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			if (len(r.Errors) > 0) != tc.failed || len(r.Entries) != 0 {
				t.Fatalf("result=%+v", r)
			}
		})
	}
}

func TestAlertTransportLargeFieldDeadline(t *testing.T) {
	scope := alertScope(t)
	scope.AlertmanagerURL = "https://alerts.example.invalid"
	body := `[{"fingerprint":"id","labels":{"namespace":"ns","pod":"api-pod"},"startsAt":"2026-11-11T11:00:00Z","endsAt":"2026-11-11T13:00:00Z","annotations":{"summary":"` + strings.Repeat("x", 6<<20) + ` token=hidden"}}]`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := CollectAlerts(ctx, scope, alertDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	t.Logf("6 MiB alert body elapsed: %s", time.Since(start))
	if r.Summary.Status != "timeout" || len(r.Entries) != 0 || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("elapsed=%s result=%+v", time.Since(start), r)
	}
}

func TestFiringAlertsProjectionDeadline(t *testing.T) {
	alert := alertRecord{Fingerprint: "id", Labels: map[string]string{"namespace": "ns", "pod": "api-pod"}, Annotations: map[string]string{"summary": strings.Repeat("x", 6<<20) + " token=hidden"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := projectAlert(ctx, alert, timelineScope().Start, timelineScope().End, "/api/v2/alerts")
	t.Logf("6 MiB projected alert field elapsed: %s", time.Since(start))
	if err != context.DeadlineExceeded || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("elapsed=%s error=%v", time.Since(start), err)
	}
}

func TestAlertTransportFailures(t *testing.T) {
	t.Run("route and exact request", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/prefix/api/v2/alerts" || r.URL.RawQuery != "active=true&inhibited=true&silenced=true&unprocessed=true" {
				t.Errorf("request=%s %s", r.Method, r.URL)
			}
			_, _ = io.WriteString(w, "[]")
		}))
		defer server.Close()
		s := timelineScope()
		s.AlertmanagerURL = server.URL + "/prefix/"
		r := CollectAlerts(context.Background(), s, server.Client())
		if len(r.Errors) != 0 {
			t.Fatalf("result=%+v", r)
		}
	})
	for _, tc := range []struct {
		name       string
		status     int
		body, code string
	}{
		{"unauthorized", 401, "token=body-hidden", "denied"}, {"forbidden", 403, "password=body-hidden", "denied"}, {"server", 500, "body-hidden", "error"},
		{"malformed", 200, `[{"labels":"body-hidden"}]`, "error"}, {"trailing", 200, "[] body-hidden", "error"}, {"null", 200, "null", "error"},
		{"oversized", 200, strings.Repeat(" ", 16<<20) + "[]", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			s := timelineScope()
			s.AlertmanagerURL = server.URL + "?token=url-hidden"
			r := CollectAlerts(context.Background(), s, server.Client())
			if r.Summary.Status != tc.code || len(r.Errors) == 0 || len(r.Entries) != 0 || !hasGap(r, tc.code) {
				t.Fatalf("result=%+v", r)
			}
			if strings.Contains(fmt.Sprint(r), "hidden") {
				t.Fatalf("unsafe result=%+v", r)
			}
			if tc.name == "oversized" && !strings.Contains(strings.Join(r.Errors, " "), "16 MiB") {
				t.Fatalf("errors=%v", r.Errors)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer server.Close()
		s := timelineScope()
		s.AlertmanagerURL = server.URL
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		r := CollectAlerts(ctx, s, server.Client())
		if r.Summary.Status != "timeout" || len(r.Errors) == 0 {
			t.Fatalf("result=%+v", r)
		}
	})
	t.Run("redirect never reaches second origin or mutates client", func(t *testing.T) {
		var reached, callback atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1); _, _ = io.WriteString(w, "[]") }))
		defer target.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"?token=redirect-hidden", http.StatusFound)
		}))
		defer server.Close()
		client := server.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { callback.Add(1); return nil }
		s := timelineScope()
		s.AlertmanagerURL = server.URL
		r := CollectAlerts(context.Background(), s, client)
		if reached.Load() != 0 || callback.Load() != 0 || len(r.Errors) == 0 || strings.Contains(fmt.Sprint(r), "redirect-hidden") {
			t.Fatalf("reached=%d callback=%d result=%+v", reached.Load(), callback.Load(), r)
		}
		_ = client.CheckRedirect(nil, nil)
		if callback.Load() != 1 {
			t.Fatal("caller client mutated")
		}
	})
	t.Run("tls verified", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "[]") }))
		defer server.Close()
		s := timelineScope()
		s.AlertmanagerURL = server.URL
		if r := CollectAlerts(context.Background(), s, nil); len(r.Errors) == 0 {
			t.Fatalf("untrusted TLS accepted: %+v", r)
		}
	})
	for _, endpoint := range []string{"https://user:password-hidden@example.invalid", "://broken", "file:///tmp/alerts"} {
		s := timelineScope()
		s.AlertmanagerURL = endpoint
		r := CollectAlerts(context.Background(), s, nil)
		if len(r.Errors) == 0 || strings.Contains(fmt.Sprint(r), "password-hidden") {
			t.Fatalf("result=%+v", r)
		}
	}
	t.Run("missing", func(t *testing.T) {
		r := CollectAlerts(context.Background(), timelineScope(), nil)
		if len(r.Errors) != 0 || !hasGap(r, "missing") {
			t.Fatalf("result=%+v", r)
		}
	})
}
