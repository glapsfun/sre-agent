package prom

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/glapsfun/sre-agent/internal/discover"
	"github.com/glapsfun/sre-agent/internal/executil"
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
	var requests, responses atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		defer responses.Add(1)
		if r.URL.Path != "/api/v1/namespaces/monitoring/services/prometheus-main:9090/proxy/api/v1/query_range" || r.URL.Query().Get("query") != "broken" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"invalid parameter query: parse error unexpected identifier password=hidden"}`)
	}))
	defer server.Close()
	dir := installActualKubectl(t, server.URL)
	sampleCtx, stopSample := context.WithCancel(context.Background())
	defer stopSample()
	sample := sampleKubectlStartup(sampleCtx, dir)
	started := time.Now()
	_, err := selectedService().Query(context.Background(), Options{Query: "broken", Context: "pinned", Timeout: 5 * time.Second})
	elapsed := time.Since(started)
	stopSample()
	var observations []string
	for observation := range sample {
		observations = observation
	}
	t.Logf("query elapsed=%s upstream_requests=%d handlers_completed=%d", elapsed, requests.Load(), responses.Load())
	if err == nil || !strings.Contains(err.Error(), "parse error unexpected identifier") || strings.Contains(err.Error(), "hidden") || !strings.Contains(err.Error(), "BEGIN EXTERNAL DATA") {
		for _, observation := range observations {
			t.Logf("startup sample: %s", safety.NewRedactor().Redact(observation))
		}
		logActualKubectlDiagnostics(t, dir)
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
exec "$PROM_REAL_KUBECTL" "$@" > "$PROM_FIXTURE/proxy.stdout" 2> "$PROM_FIXTURE/proxy.stderr"
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

func logActualKubectlDiagnostics(t *testing.T, dir string) {
	t.Helper()
	real := os.Getenv("PROM_REAL_KUBECTL")
	resolved, resolveErr := filepath.EvalSymlinks(real)
	t.Logf("kubectl executable=%q resolved=%q resolve_error=%v", real, resolved, resolveErr)
	// Read executable pages only after failure so this cannot warm startup.
	info, buildErr := buildinfo.ReadFile(real)
	if buildErr != nil {
		t.Logf("kubectl offline build info: %v", buildErr)
	} else {
		t.Logf("kubectl offline build: go=%q main=%q version=%q", info.GoVersion, info.Main.Path, info.Main.Version)
		for _, dependency := range info.Deps {
			if dependency.Path == "k8s.io/kubectl" || dependency.Path == "k8s.io/client-go" {
				t.Logf("kubectl module: %s@%s", dependency.Path, dependency.Version)
			}
		}
	}
	_, launchErr := os.Stat(filepath.Join(dir, "proxy.pid"))
	socket, socketErr := os.ReadFile(filepath.Join(dir, "proxy.socket"))
	var socketPresent, directoryPresent bool
	if socketErr == nil {
		_, err := os.Stat(string(socket))
		socketPresent = err == nil
		_, err = os.Stat(filepath.Dir(string(socket)))
		directoryPresent = err == nil
	}
	t.Logf("wrapper_started=%t socket_recorded=%t socket_present_after_query=%t proxy_directory_present_after_query=%t", launchErr == nil, socketErr == nil, socketPresent, directoryPresent)
	// Direct file redirection preserves exec and process-group cleanup. Only
	// fixture-owned logs are read, with capped and redacted diagnostic output.
	for _, name := range []string{"proxy.stdout", "proxy.stderr"} {
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Logf("%s unavailable: %v", name, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(file, 4097))
		file.Close()
		truncated := len(body) > 4096
		if truncated {
			body = body[:4096]
		}
		t.Logf("%s truncated=%t read_error=%v: %q", name, truncated, readErr, safety.NewRedactor().Redact(string(body)))
	}
	// Query first so version inspection cannot warm up a slow proxy launch.
	version := (executil.Runner{Timeout: time.Second}).Run(context.Background(), os.Getenv("PROM_REAL_KUBECTL"), "version", "--client", "-o", "json")
	if len(version.Stdout) > 4096 {
		version.Stdout = version.Stdout[:4096]
	}
	t.Logf("kubectl client version: %q error=%v", version.Stdout, version.Err)
}

func sampleKubectlStartup(ctx context.Context, dir string) <-chan []string {
	result := make(chan []string, 1)
	go func() {
		defer close(result)
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			if ctx.Err() == nil {
				result <- kubectlStartupObservations(dir, runtime.GOOS)
			}
		}
	}()
	return result
}

func kubectlStartupObservations(dir, platform string) []string {
	if platform != "linux" {
		return []string{"proc observations unsupported on " + platform}
	}
	file, err := os.Open(filepath.Join(dir, "proxy.pid"))
	if err != nil {
		return []string{fmt.Sprintf("pid unavailable: %v", err)}
	}
	body, readErr := io.ReadAll(io.LimitReader(file, 32))
	file.Close()
	pid, parseErr := strconv.Atoi(string(body))
	if readErr != nil || parseErr != nil || pid <= 0 || len(body) == 32 {
		return []string{"invalid pid in fixture marker"}
	}
	root := filepath.Join("/proc", strconv.Itoa(pid))
	executable, exeErr := os.Readlink(filepath.Join(root, "exe"))
	observations := []string{fmt.Sprintf("pid=%d exe=%q error=%v", pid, executable, exeErr)}
	for _, name := range []string{"stat", "wchan"} {
		file, err := os.Open(filepath.Join(root, name))
		if err != nil {
			observations = append(observations, fmt.Sprintf("%s unavailable: %v", name, err))
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(file, 4096))
		file.Close()
		observations = append(observations, fmt.Sprintf("%s=%q error=%v", name, body, readErr))
	}
	return observations
}

func proxySocket(t *testing.T, dir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "proxy.socket"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestKubectlStartupSamplerCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		started := time.Now()
		result := sampleKubectlStartup(ctx, t.TempDir())
		cancel()
		if observations := <-result; len(observations) != 0 {
			t.Fatalf("cancelled sampler performed observations: %v", observations)
		}
		if time.Since(started) != 0 {
			t.Fatal("sampler waited for its timer after cancellation")
		}
	})
}

func TestKubectlStartupObservationsUnavailable(t *testing.T) {
	dir := t.TempDir()
	if observations := kubectlStartupObservations(dir, "darwin"); !strings.Contains(strings.Join(observations, " "), "unsupported") {
		t.Fatalf("unsupported platform was not reported: %v", observations)
	}
	if observations := kubectlStartupObservations(dir, "linux"); !strings.Contains(strings.Join(observations, " "), "pid unavailable") {
		t.Fatalf("missing pid was not reported: %v", observations)
	}
	for _, pid := range []string{"0", "../self", strings.Repeat("1", 64), "2147483647"} {
		if err := os.WriteFile(filepath.Join(dir, "proxy.pid"), []byte(pid), 0600); err != nil {
			t.Fatal(err)
		}
		observations := strings.Join(kubectlStartupObservations(dir, "linux"), " ")
		if !strings.Contains(observations, "invalid pid") && !strings.Contains(observations, "no such file or directory") {
			t.Fatalf("invalid or missing process not reported for %q: %s", pid, observations)
		}
	}
}

func TestKubectlStartupSamplerObservesProcess(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "proxy.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		var observations []string
		for sample := range sampleKubectlStartup(context.Background(), dir) {
			observations = append(observations, sample...)
		}
		if len(observations) == 0 || time.Since(started) != time.Second {
			t.Fatalf("missing delayed observation: elapsed=%s observations=%v", time.Since(started), observations)
		}
		if runtime.GOOS == "linux" {
			for _, observation := range observations {
				if !strings.Contains(observation, "error=<nil>") {
					t.Fatalf("could not inspect live fixture process: %s", observation)
				}
			}
		}
	})
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
