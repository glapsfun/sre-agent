package prom

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/discover"
	"github.com/glapsfun/sre-agent/internal/safety"
)

// Run the test binary as the fake kubectl proxy so lifecycle tests exercise a
// real child process and Unix HTTP listener without an additional dependency.
func TestKubectlProxyHelper(t *testing.T) {
	if os.Getenv("PROM_PROXY_HELPER") != "1" {
		return
	}
	dir := os.Getenv("PROM_FIXTURE")
	socket := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "--unix-socket=") {
			socket = strings.TrimPrefix(arg, "--unix-socket=")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "proxy.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(8)
	}
	if err := os.WriteFile(filepath.Join(dir, "proxy.socket"), []byte(socket), 0600); err != nil {
		os.Exit(8)
	}
	switch os.Getenv("PROM_PROXY_MODE") {
	case "startup failure":
		os.Exit(4)
	case "startup timeout":
		time.Sleep(time.Hour)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(5)
	}
	err = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, err := os.OpenFile(filepath.Join(dir, "requests"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(8)
		}
		fmt.Fprintln(file, r.URL.String())
		file.Close()
		switch os.Getenv("PROM_PROXY_MODE") {
		case "child death":
			os.Exit(6)
		case "request timeout":
			<-r.Context().Done()
			return
		}
		if os.Getenv("PROM_DENIED") == "true" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		body, err := os.ReadFile(filepath.Join(dir, "response"))
		if err != nil {
			os.Exit(8)
		}
		fmt.Fprint(w, string(body))
	}))
	if err != nil {
		os.Exit(7)
	}
	os.Exit(0)
}

func selectedService() Service {
	s := fixedService()
	s.Discover = func(ctx context.Context, opts discover.Options) (discover.Report, error) {
		if opts.Context != "pinned" {
			return discover.Report{}, fmt.Errorf("unexpected context")
		}
		return discover.Report{Probes: map[string]discover.ProbeResult{"obs": {Findings: discover.Findings{Obs: &discover.Observability{EndpointInventoryComplete: true, Endpoints: []discover.Endpoint{{Component: "prometheus", Namespace: "monitoring", Service: "prometheus-main", Ports: []int{9090}}}}}}}}, nil
	}
	return s
}

func TestActualKubectlPreservesPrometheusHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/monitoring/services/prometheus-main:9090/proxy/api/v1/query_range" || r.URL.Query().Get("query") != "broken" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"invalid parameter query: parse error unexpected identifier password=hidden"}`)
	}))
	defer server.Close()
	dir := installActualKubectl(t, server.URL)
	_, err := selectedService().Query(context.Background(), Options{Query: "broken", Context: "pinned", Timeout: 5 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "parse error unexpected identifier") || strings.Contains(err.Error(), "hidden") || !strings.Contains(err.Error(), "BEGIN EXTERNAL DATA") {
		t.Fatalf("error body lost or unsafe: %v", err)
	}
	assertProxyStopped(t, dir)
}

func installActualKubectl(t *testing.T, endpoint string) string {
	t.Helper()
	real, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("integration requires installed kubectl")
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "kubeconfig")
	body := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: %s\ncontexts:\n- name: pinned\n  context:\n    cluster: fixture\n    user: fixture\ncurrent-context: wrong\nusers:\n- name: fixture\n  user: {}\n", endpoint)
	if err := os.WriteFile(config, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$@" > "$PROM_FIXTURE/calls"
printf '%s' "$$" > "$PROM_FIXTURE/proxy.pid"
for arg in "$@"; do
  case "$arg" in --unix-socket=*) printf '%s' "${arg#--unix-socket=}" > "$PROM_FIXTURE/proxy.socket";; esac
done
exec "$PROM_REAL_KUBECTL" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROM_REAL_KUBECTL", real)
	t.Setenv("PROM_FIXTURE", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KUBECONFIG", config)
	t.Setenv("SRE_SKIP_CLUSTER", "")
	return dir
}

func proxySocket(t *testing.T, dir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "proxy.socket"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func assertProxyStopped(t *testing.T, dir string) {
	t.Helper()
	socket := proxySocket(t, dir)
	pidText, err := os.ReadFile(filepath.Join(dir, "proxy.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidText))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		t.Errorf("proxy process %d still exists: %v", pid, err)
	}
	if _, err := os.Stat(filepath.Dir(socket)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("proxy directory remains: %s (%v)", filepath.Dir(socket), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "proxy.pid")); err != nil {
		t.Error("cleanup removed a directory it did not own")
	}
}

func TestProxyLifecycleCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, mode, body, want string
		timeout                time.Duration
	}{
		{"success", "", matrix, "", 5 * time.Second},
		{"startup failure", "startup failure", matrix, "failed to start", 5 * time.Second},
		{"child death", "child death", matrix, "proxy", 5 * time.Second},
		{"startup timeout", "startup timeout", matrix, "deadline", 500 * time.Millisecond},
		{"request timeout", "request timeout", matrix, "deadline", 500 * time.Millisecond},
		{"oversize", "", strings.Repeat("x", safety.MaxResponseBytes+1), "8 MiB", 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := installKubectl(t, tc.body, oneService, false)
			t.Setenv("PROM_PROXY_MODE", tc.mode)
			opts := queryOptions("")
			opts.Context = "pinned"
			opts.Timeout = tc.timeout
			_, err := selectedService().Query(context.Background(), opts)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("%v", err)
			}
			if tc.want == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("untyped deadline: %v", err)
			}
			assertProxyStopped(t, dir)
		})
	}
}

func TestActualKubectlDefaultGridParity(t *testing.T) {
	body := defaultGridMatrix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer server.Close()
	dir := installActualKubectl(t, server.URL)
	s := selectedService()
	s.Now = func() time.Time { return time.Unix(3600, 0) }
	direct, err := s.Query(context.Background(), Options{URL: server.URL, Query: "up", Top: 10})
	if err != nil {
		t.Fatal(err)
	}
	proxied, err := s.Query(context.Background(), Options{Context: "pinned", Query: "up", Top: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(direct, proxied) || len(proxied.Series) != 10 || proxied.OmittedSeries != 490 {
		t.Fatal("actual-kubectl default-grid parity failed")
	}
	assertProxyStopped(t, dir)
}

func TestActualKubectlProxyFiltersAndCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	dir := installActualKubectl(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		opts := queryOptions("")
		opts.Context = "pinned"
		opts.Timeout = 5 * time.Second
		_, err := selectedService().Query(ctx, opts)
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("query ended before request: %v", err)
	case <-time.After(4 * time.Second):
		t.Fatal("proxy never reached fixture")
	}
	socket := proxySocket(t, dir)
	info, err := os.Stat(filepath.Dir(socket))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory mode: %v %v", info, err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	path := "/api/v1/namespaces/monitoring/services/prometheus-main:9090/proxy/api/v1/query_range"
	for _, tc := range []struct{ method, path, host string }{
		{"GET", path + "/", "localhost"}, {"GET", "/api/v1/namespaces", "localhost"}, {"GET", path, "other.example"},
		{"POST", path, "localhost"}, {"HEAD", path, "localhost"}, {"G", path, "localhost"}, {"GE", path, "localhost"}, {"GETX", path, "localhost"}, {"CUSTOM", path, "localhost"},
	} {
		request, err := http.NewRequest(tc.method, "http://localhost"+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = tc.host
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("filter accepted %+v: status %d", tc, response.StatusCode)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("unexpected upstream requests: %d", hits.Load())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	assertProxyStopped(t, dir)
}

func TestActualKubectlHTTPFailuresAndRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, status := range []int{http.StatusForbidden, http.StatusBadGateway, http.StatusFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				fmt.Fprint(w, "upstream password=hidden")
			}))
			defer server.Close()
			dir := installActualKubectl(t, server.URL)
			opts := queryOptions("")
			opts.Context = "pinned"
			_, err := selectedService().Query(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) || !strings.Contains(err.Error(), "--url") || strings.Contains(err.Error(), "hidden") {
				t.Fatalf("HTTP error: %v", err)
			}
			assertProxyStopped(t, dir)
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("proxy followed a redirect")
	}
}
