package prom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/discover"
	"github.com/glapsfun/sre-agent/internal/safety"
)

const matrix = `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"api","token":"hidden-label"},"values":[[0,"2"],[1,"2"],[2,"2"],[3,"10"],[4,"10"],[5,"10"]]}]}}`

func queryOptions(endpoint string) Options {
	return Options{URL: endpoint, Query: `sum(rate(requests{job="a&b"}[5m]))`, Range: 5 * time.Second, Step: time.Second}
}
func fixedService() Service { return Service{Now: func() time.Time { return time.Unix(5, 0) }} }

func TestHTTPEncodingSummaryAndRedaction(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/api/v1/query_range" || r.URL.Query().Get("query") != queryOptions("").Query || r.URL.Query().Get("start") != "0" || r.URL.Query().Get("end") != "5" || r.URL.Query().Get("step") != "1000ms" {
			t.Errorf("request: %s", r.URL)
		}
		fmt.Fprint(w, matrix)
	}))
	defer server.Close()
	r, err := fixedService().Query(context.Background(), queryOptions(server.URL+"/prefix/"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Series) != 1 || *r.Series[0].Mean != 6 || r.Series[0].Labels["token"] != "[REDACTED]" {
		t.Fatalf("report: %+v", r)
	}
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "hidden-label") || strings.Contains(string(encoded), "values") {
		t.Fatalf("raw data retained: %s", encoded)
	}
}

func TestHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"api", `{"status":"error","errorType":"bad_data","error":"invalid query password=hidden"}`, "invalid query", 422},
		{"http", "password=hidden", "HTTP 503", 503},
		{"warnings", `{"status":"success","warnings":["partial response password=hidden"],"data":{"resultType":"matrix","result":[]}}`, "warning", 200},
		{"null", `null`, "invalid", 200},
		{"missing result", `{"status":"success","data":{"resultType":"matrix"}}`, "invalid", 200},
		{"vector", `{"status":"success","data":{"resultType":"vector","result":[]}}`, "matrix", 200},
		{"bad labels", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":1},"values":[]}]}}`, "invalid", 200},
		{"null labels", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":null,"values":[]}]}}`, "invalid", 200},
		{"null value", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[0,null]]}]}}`, "invalid", 200},
		{"numeric value", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[0,1]]}]}}`, "invalid", 200},
		{"string timestamp", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[["0","1"]]}]}}`, "invalid", 200},
		{"extra sample field", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[0,"1",3]]}]}}`, "invalid", 200},
		{"histogram", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"histograms":[[0,{}]]}]}}`, "histogram", 200},
		{"trailing", matrix + ` {}`, "invalid", 200},
		{"oversize", strings.Repeat("x", safety.MaxResponseBytes+1), "8 MiB", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			r, err := fixedService().Query(context.Background(), queryOptions(server.URL))
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "hidden") || len(r.Series) != 0 {
				t.Fatalf("result=%+v error=%v", r, err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeadlineAndTransportCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	opts := queryOptions(server.URL)
	opts.Timeout = 20 * time.Millisecond
	_, err := fixedService().Query(context.Background(), opts)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	s := fixedService()
	s.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("failed %s", r.URL) })}
	_, err = s.Query(context.Background(), queryOptions("http://user:hidden@localhost/prefix?token=hidden-query"))
	if err == nil || strings.Contains(err.Error(), "hidden") || strings.Contains(err.Error(), "user") {
		t.Fatalf("credentials leaked: %v", err)
	}
}

func TestNonfiniteAndFractionalResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","infos":["info token=hidden"],"data":{"resultType":"matrix","result":[{"metric":{},"values":[[0.125,"NaN"],[0.375,"+Inf"],[0.625,"-Inf"],[0.875,"4"]]}]}}`)
	}))
	defer server.Close()
	s := Service{Now: func() time.Time { return time.Unix(0, 875000000) }}
	r, err := s.Query(context.Background(), Options{URL: server.URL, Query: "up", Range: 750 * time.Millisecond, Step: 250 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Infos) != 1 || strings.Contains(r.Infos[0], "hidden") || r.Series[0].NaNCount != 1 || r.Series[0].MissingSteps != 0 || *r.Series[0].Mean != 4 {
		t.Fatalf("%+v", r)
	}
	if _, err := json.Marshal(r); err != nil {
		t.Fatal(err)
	}
}

func manySeries(reverse bool) string {
	series := make([]string, 500)
	for i := range series {
		n := i
		if reverse {
			n = 499 - i
		}
		series[i] = fmt.Sprintf(`{"metric":{"job":"job-%03d"},"values":[[0,"0"],[5,"%d"]]}`, n, n/2)
	}
	return `{"status":"success","data":{"resultType":"matrix","result":[` + strings.Join(series, ",") + `]}}`
}

func TestTopSeriesStableAndOmissions(t *testing.T) {
	var prior Report
	for _, reverse := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, manySeries(reverse)) }))
		opts := queryOptions(server.URL)
		opts.Top = 10
		r, err := fixedService().Query(context.Background(), opts)
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Series) != 10 || r.OmittedSeries != 490 || r.Series[0].Labels["job"] != "job-498" || r.Series[1].Labels["job"] != "job-499" {
			t.Fatalf("bad top: %+v", r)
		}
		if reverse && !reflect.DeepEqual(prior, r) {
			t.Fatal("shuffled input changed ranking")
		}
		prior = r
	}
}

func defaultGridMatrix() string {
	var body strings.Builder
	body.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[`)
	for series := 0; series < 500; series++ {
		if series > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"metric":{"job":"job-%03d","token":"hidden-label"},"values":[`, series)
		for step := 0; step <= 120; step++ {
			if step > 0 {
				body.WriteByte(',')
			}
			value := 0
			if step >= 60 {
				value = series
			}
			fmt.Fprintf(&body, `[%d,"%d"]`, step*30, value)
		}
		body.WriteString(`]}`)
	}
	body.WriteString(`]}}`)
	return body.String()
}

func TestDefaultGrid500SeriesHTTPAndProxy(t *testing.T) {
	body := defaultGridMatrix()
	t.Logf("500 series, 60,500 samples, %d response bytes", len(body))
	installKubectl(t, body, oneService, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") != "0" || r.URL.Query().Get("end") != "3600" || r.URL.Query().Get("step") != "30000ms" {
			t.Errorf("default grid: %s", r.URL)
		}
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	s := Service{Now: func() time.Time { return time.Unix(3600, 0) }}
	var direct Report
	for _, endpoint := range []string{server.URL, ""} {
		report, err := s.Query(context.Background(), Options{URL: endpoint, Query: "up", Top: 10})
		if err != nil {
			t.Fatalf("endpoint %q: %v", endpoint, err)
		}
		if len(report.Series) != 10 || report.OmittedSeries != 490 {
			t.Fatalf("listed=%d omitted=%d", len(report.Series), report.OmittedSeries)
		}
		for i, series := range report.Series {
			if series.Labels["job"] != fmt.Sprintf("job-%03d", 499-i) || series.Labels["token"] != "[REDACTED]" || series.MissingSteps != 0 || series.Change == nil || !series.Change.Time.Equal(time.Unix(1800, 0)) {
				t.Fatalf("series %d: %+v", i, series)
			}
		}
		if endpoint != "" {
			direct = report
		} else if !reflect.DeepEqual(direct, report) {
			t.Fatal("default-grid transport summaries differ")
		}
	}
}

func TestMatrixDecoderRetainsValidationAndBounds(t *testing.T) {
	full := defaultGridMatrix()
	dense := `[` + strings.Repeat(`0,`, safety.MaxJSONStructure) + `0]`
	for _, tc := range []struct{ name, body, want string }{
		{"late warning", strings.TrimSuffix(full, `}`) + `,"warnings":["partial response token=hidden"]}`, "warning"},
		{"late invalid metadata", strings.TrimSuffix(full, `}`) + `,"infos":123}`, "invalid"},
		{"truncated matrix", full[:len(full)-3], "invalid"},
		{"trailing matrix", full + ` {}`, "invalid"},
		{"result type", `{"status":"success","data":{"resultType":"matrix","result":{}}}`, "invalid"},
		{"null data", `{"status":"success","data":null}`, "invalid"},
		{"dense series", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[],"unknown":` + dense + `}]}}`, "65536"},
		{"dense metadata", `{"status":"success","unknown":` + dense + `,"data":{"resultType":"matrix","result":[]}}`, "65536"},
		{"oversize", strings.Repeat(" ", safety.MaxResponseBytes) + `{}`, "8 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := decode(context.Background(), []byte(tc.body), time.Unix(0, 0), time.Unix(3600, 0), 30*time.Second, 10, safety.NewRedactor())
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "hidden") || len(report.Series) != 0 {
				t.Fatalf("report=%+v error=%v", report, err)
			}
		})
	}
}

func TestLargeMatrixDecodeDeadline(t *testing.T) {
	body := []byte(defaultGridMatrix())
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	report, err := decode(ctx, body, time.Unix(0, 0), time.Unix(3600, 0), 30*time.Second, 10, safety.NewRedactor())
	if !errors.Is(err, context.DeadlineExceeded) || len(report.Series) != 0 {
		t.Fatalf("report=%+v error=%v", report, err)
	}
}

func installKubectl(t *testing.T, body, services string, denied bool) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{"response": body, "services": services} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$PROM_FIXTURE/calls"
if [ "$1 $2" = 'config current-context' ]; then echo pinned; exit; fi
if [ "$1 $2" != '--context pinned' ]; then exit 9; fi
shift 2
if [ "$1" = proxy ]; then
  PROM_PROXY_HELPER=1 exec "$PROM_TEST_BINARY" -test.run=^TestKubectlProxyHelper$ -- "$@"
fi
if [ "$1 $2" = 'get services' ]; then cat "$PROM_FIXTURE/services"; exit; fi
printf '{"items":[]}\n'
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PROM_FIXTURE", dir)
	t.Setenv("PROM_DENIED", fmt.Sprint(denied))
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROM_TEST_BINARY", program)
	t.Setenv("PROM_PROXY_MODE", "")
	t.Setenv("SRE_KUBE_CONTEXT", "")
	t.Setenv("SRE_SKIP_CLUSTER", "")
	return dir
}

const oneService = `{"items":[{"metadata":{"name":"prometheus-main","namespace":"monitoring"},"spec":{"ports":[{"port":80},{"port":9090}]}}]}`

func TestDiscoveredProxyMatchesDirectAndPinsOnce(t *testing.T) {
	for _, body := range []string{matrix, manySeries(true)} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			dir := installKubectl(t, body, oneService, false)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			opts := queryOptions(server.URL)
			opts.Top = 10
			direct, err := fixedService().Query(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			opts.URL = ""
			proxied, err := fixedService().Query(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(direct, proxied) {
				t.Fatalf("different transport reports: %+v %+v", direct, proxied)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(calls), "config current-context") != 1 {
				t.Fatalf("context calls: %s", calls)
			}
			for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if call == "config current-context" {
					continue
				}
				if !strings.HasPrefix(call, "--context pinned ") {
					t.Fatalf("unpinned: %s", call)
				}
			}
			if !strings.Contains(string(calls), "--context pinned proxy --unix-socket=") || strings.Contains(string(calls), "get --raw") {
				t.Fatalf("unexpected transport: %s", calls)
			}
			requests, err := os.ReadFile(filepath.Join(dir, "requests"))
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(strings.TrimSpace(string(requests)))
			if err != nil {
				t.Fatal(err)
			}
			if u.Path != "/api/v1/namespaces/monitoring/services/prometheus-main:9090/proxy/api/v1/query_range" || u.Query().Get("query") != opts.Query {
				t.Fatalf("proxy: %s", requests)
			}
		})
	}
}

func TestDiscoveryFailuresAndExplicitURLGuidance(t *testing.T) {
	for _, tc := range []struct {
		name, services string
		denied         bool
	}{
		{"missing", `{"items":[]}`, false},
		{"multiple", `{"items":[{"metadata":{"name":"prometheus-main","namespace":"monitoring"},"spec":{"ports":[{"port":9090}]}},{"metadata":{"name":"prometheus-other","namespace":"other"},"spec":{"ports":[{"port":9090}]}}]}`, false},
		{"ports", strings.Replace(oneService, `{"port":9090}`, `{"port":443}`, 1), false},
		{"incomplete", strings.Replace(oneService, `"ports":[{"port":80},{"port":9090}]`, `"ports":[]`, 1), false},
		{"denied", oneService, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installKubectl(t, matrix, tc.services, tc.denied)
			_, err := fixedService().Query(context.Background(), queryOptions(""))
			if err == nil || !strings.Contains(err.Error(), "--url") {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestReadinessHintIsNeverQueried(t *testing.T) {
	installKubectl(t, matrix, oneService, false)
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; fmt.Fprint(w, matrix) }))
	defer server.Close()
	s := fixedService()
	s.Discover = func(ctx context.Context, opts discover.Options) (discover.Report, error) {
		return discover.Report{Probes: map[string]discover.ProbeResult{"obs": {Findings: discover.Findings{Obs: &discover.Observability{EndpointInventoryComplete: true, Endpoints: []discover.Endpoint{{Component: "prometheus", Namespace: "monitoring", Service: "prometheus-main", Ports: []int{9090}, ReadinessURL: server.URL}}}}}}}, nil
	}
	if _, err := s.Query(context.Background(), queryOptions("")); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatal("queried forwarding hint")
	}
}

func TestOptionsValidation(t *testing.T) {
	for _, opts := range []Options{{}, {Query: "up", Range: -1}, {Query: "up", Step: -1}, {Query: "up", Timeout: -1}, {Query: "up", Top: -1}, {Query: "up", URL: "ftp://example.com"}} {
		if _, err := Query(context.Background(), opts); err == nil {
			t.Fatalf("accepted %+v", opts)
		}
	}
}

func TestQueryRejectsSubmillisecondGridBeforeTransport(t *testing.T) {
	for _, tc := range []struct {
		name       string
		span, step time.Duration
	}{
		{"range", time.Second + 100*time.Microsecond, 250 * time.Millisecond},
		{"step", 5 * time.Millisecond, 1500 * time.Microsecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, endpoint := range []string{"http://example.invalid", ""} {
				dir := installKubectl(t, matrix, oneService, false)
				calls := 0
				s := fixedService()
				s.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") })}
				_, err := s.Query(context.Background(), Options{URL: endpoint, Query: "up", Range: tc.span, Step: tc.step})
				if err == nil || !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), "whole milliseconds") || calls != 0 {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
				if _, err := os.Stat(filepath.Join(dir, "calls")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("invalid grid executed kubectl")
				}
			}
		})
	}
}

func TestSupportedFractionalGridMatchesPrometheusMilliseconds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parseTime := func(text string) int64 {
			value, err := strconv.ParseFloat(text, 64)
			if err != nil {
				t.Error(err)
			}
			seconds, fraction := math.Modf(value)
			return time.Unix(int64(seconds), int64(math.Round(fraction*1000)/1000*float64(time.Second))).UnixMilli()
		}
		query := r.URL.Query()
		start, end := parseTime(query.Get("start")), parseTime(query.Get("end"))
		stepSeconds, err := strconv.ParseFloat(query.Get("step"), 64)
		step := int64(time.Duration(stepSeconds*float64(time.Second)) / time.Millisecond)
		if err != nil {
			step, err = strconv.ParseInt(strings.TrimSuffix(query.Get("step"), "ms"), 10, 64)
			if err != nil || !strings.HasSuffix(query.Get("step"), "ms") {
				t.Error("invalid millisecond duration")
			}
		}
		if step <= 0 {
			t.Error("unsupported grid reached server")
			http.Error(w, "bad step", 400)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[`)
		for at := start; at <= end; at += step {
			if at > start {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `[%d.%03d,"1"]`, at/1000, at%1000)
		}
		fmt.Fprint(w, `]}]}}`)
	}))
	defer server.Close()
	now := time.Unix(1794393000, 123456789)
	for _, tc := range []struct{ span, step time.Duration }{{1001 * time.Millisecond, 250 * time.Millisecond}, {5 * time.Millisecond, time.Millisecond}, {3003 * time.Millisecond, 1001 * time.Millisecond}} {
		s := Service{Now: func() time.Time { return now }}
		report, err := s.Query(context.Background(), Options{URL: server.URL, Query: "vector(1)", Range: tc.span, Step: tc.step})
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Series) != 1 || report.Series[0].MissingSteps != 0 || *report.Series[0].Mean != 1 || !report.Start.Equal(now.Truncate(time.Millisecond).Add(-tc.span)) {
			t.Fatalf("compatible grid: %+v", report)
		}
	}
}

func TestProxyEncodesExactMillisecondStep(t *testing.T) {
	dir := installKubectl(t, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[0,"1"],[1.001,"1"],[2.002,"1"],[3.003,"1"]]}]}}`, oneService, false)
	s := Service{Now: func() time.Time { return time.UnixMilli(3003) }}
	report, err := s.Query(context.Background(), Options{Context: "pinned", Query: "up", Range: 3003 * time.Millisecond, Step: 1001 * time.Millisecond})
	if err != nil || len(report.Series) != 1 || report.Series[0].MissingSteps != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	requests, err := os.ReadFile(filepath.Join(dir, "requests"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(strings.TrimSpace(string(requests)))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("step") != "1001ms" {
		t.Fatalf("inexact wire step: %s", u.Query().Get("step"))
	}
}

func TestTimestampRejectsUnboundedExponents(t *testing.T) {
	for _, raw := range []string{"0e1000", "0e-1000"} {
		if _, err := sampleTime(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted pathological timestamp %s", raw)
		}
	}
	tm, err := sampleTime(json.RawMessage("1794393000.123456789"))
	if err != nil || tm.Unix() != 1794393000 || tm.Nanosecond() != 123456789 {
		t.Fatalf("timestamp=%v err=%v", tm, err)
	}
}

func TestTopRankingDoesNotSaturateLargeChanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"a-smaller"},"values":[[0,"%g"],[5,"%g"]]},{"metric":{"job":"z-larger"},"values":[[0,"%g"],[5,"%g"]]}]}}`, -math.MaxFloat64, math.MaxFloat64/2, -math.MaxFloat64, math.MaxFloat64)
	}))
	defer server.Close()
	opts := queryOptions(server.URL)
	opts.Top = 1
	r, err := fixedService().Query(context.Background(), opts)
	if err != nil || r.Series[0].Labels["job"] != "z-larger" {
		t.Fatalf("large rank: %+v %v", r, err)
	}
}

func TestProxyRejectsWarningsMalformedAndOversize(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"warning", `{"status":"success","warnings":["partial results"],"data":{"resultType":"matrix","result":[]}}`, "warning"},
		{"malformed", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":null},"values":[]}]}}`, "invalid"},
		{"oversize", strings.Repeat("x", safety.MaxResponseBytes+1), "8 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installKubectl(t, tc.body, oneService, false)
			r, err := fixedService().Query(context.Background(), queryOptions(""))
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(r.Series) != 0 {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

func TestExplicitAndEnvironmentContextsStayPinned(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			dir := installKubectl(t, matrix, oneService, false)
			opts := queryOptions("")
			if explicit {
				opts.Context = "pinned"
				t.Setenv("SRE_KUBE_CONTEXT", "wrong")
			} else {
				t.Setenv("SRE_KUBE_CONTEXT", "pinned")
			}
			if _, err := fixedService().Query(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), "config current-context") || strings.Contains(string(calls), "wrong") {
				t.Fatalf("context selection: %s", calls)
			}
		})
	}
}

func TestSingleNonstandardPortAndIncompleteInventory(t *testing.T) {
	t.Run("single port", func(t *testing.T) {
		dir := installKubectl(t, matrix, strings.Replace(oneService, `{"port":80},{"port":9090}`, `{"port":8080}`, 1), false)
		if _, err := fixedService().Query(context.Background(), queryOptions("")); err != nil {
			t.Fatal(err)
		}
		calls, err := os.ReadFile(filepath.Join(dir, "calls"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(calls), "prometheus-main:8080/proxy/") {
			t.Fatalf("port: %s", calls)
		}
	})
	t.Run("omitted endpoint", func(t *testing.T) {
		installKubectl(t, matrix, oneService, false)
		s := fixedService()
		s.Discover = func(context.Context, discover.Options) (discover.Report, error) {
			return discover.Report{Probes: map[string]discover.ProbeResult{"obs": {Findings: discover.Findings{Obs: &discover.Observability{EndpointInventoryComplete: false, Endpoints: []discover.Endpoint{{Component: "prometheus", Namespace: "monitoring", Service: "prometheus-main", Ports: []int{9090}}}}}}}}, nil
		}
		if _, err := s.Query(context.Background(), queryOptions("")); err == nil || !strings.Contains(err.Error(), "incomplete") {
			t.Fatalf("%v", err)
		}
	})
}

func TestProxyDeadlineAndEncodedBasePath(t *testing.T) {
	t.Run("proxy deadline", func(t *testing.T) {
		dir := installKubectl(t, matrix, oneService, false)
		if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\nsleep 2\n"), 0700); err != nil {
			t.Fatal(err)
		}
		opts := queryOptions("")
		opts.Context = "pinned"
		opts.Timeout = 25 * time.Millisecond
		_, err := fixedService().Query(context.Background(), opts)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("encoded prefix", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.EscapedPath() != "/prefix%2Ftenant/api/v1/query_range" || r.URL.Query().Get("tenant") != "a/b" {
				t.Errorf("URL: %s", r.URL)
			}
			fmt.Fprint(w, matrix)
		}))
		defer server.Close()
		if _, err := fixedService().Query(context.Background(), queryOptions(server.URL+"/prefix%2Ftenant?tenant=a%2Fb")); err != nil {
			t.Fatal(err)
		}
	})
}
