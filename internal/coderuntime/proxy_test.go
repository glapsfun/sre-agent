package coderuntime

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/collect"
)

func testProxy(t *testing.T, ctx context.Context, target string, budget *proxyBudget) (*egressProxy, string) {
	t.Helper()
	// Darwin's TMPDIR and test names can exceed the Unix socket path limit.
	dir, err := os.MkdirTemp("/tmp", "sre-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("socket directory cleanup: %v", err)
		}
	})
	path := filepath.Join(dir, "proxy.sock")
	p, err := startProxy(ctx, path, collect.Invocation{Phase: "source", SourceID: "trusted-source", HostPort: target}, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p, path
}
func proxyConn(t *testing.T, path string) net.Conn {
	t.Helper()
	c, e := net.Dial("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}
func proxyRequest(t *testing.T, path, target string) (*http.Response, string) {
	t.Helper()
	c := proxyConn(t, path)
	fmt.Fprintf(c, "GET http://%s/data HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	r, e := http.ReadResponse(bufio.NewReader(c), nil)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(r.Body)
	r.Body.Close()
	if e != nil {
		t.Fatal(e)
	}
	return r, string(b)
}
func failedProxy(t *testing.T, p *egressProxy) {
	t.Helper()
	select {
	case <-p.Failed():
	case <-time.After(5 * time.Second):
		t.Fatal("missing terminal failure")
	}
	if p.Err() == nil {
		t.Fatal("missing sticky error")
	}
}
func TestProxyHTTP(t *testing.T) {
	var count atomic.Int32
	var expectedHost string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.Host != expectedHost {
			t.Error("missing approved host")
		}
		if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Secret") != "" {
			t.Error("hop headers leaked")
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		io.WriteString(c, "HTTP/1.1 200 OK\r\nConnection: close, X-Remove\r\nX-Remove: private\r\nContent-Length: 12\r\n\r\nfixture-body")
	}))
	defer s.Close()
	expectedHost = strings.TrimPrefix(s.URL, "http://")
	p, path := testProxy(t, context.Background(), expectedHost, newProxyBudget())
	c := proxyConn(t, path)
	fmt.Fprintf(c, "GET %s/data HTTP/1.1\r\nHost: %s\r\nConnection: X-Secret\r\nX-Secret: hidden\r\nProxy-Authorization: secret\r\n\r\n", s.URL, strings.TrimPrefix(s.URL, "http://"))
	r, e := http.ReadResponse(bufio.NewReader(c), nil)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(r.Body)
	if e != nil || string(b) != "fixture-body" {
		t.Fatalf("body %q: %v", b, e)
	}
	if r.Header.Get("X-Remove") != "" || count.Load() != 1 {
		t.Fatalf("forwarding mismatch: headers=%v count=%d", r.Header, count.Load())
	}
	a, e := p.Close()
	if e != nil || len(a) != 1 || a[0].Permission != "network-allowed" || a[0].SourceID != "trusted-source" || a[0].Phase != "source" || a[0].At.Location() != time.UTC {
		t.Fatalf("audit %#v %v", a, e)
	}
}
func TestProxyCONNECTTLS(t *testing.T) {
	var count atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count.Add(1); io.WriteString(w, "encrypted-body") }))
	defer s.Close()
	target := strings.TrimPrefix(s.URL, "https://")
	p, path := testProxy(t, context.Background(), target, newProxyBudget())
	c := proxyConn(t, path)
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	r, e := http.ReadResponse(bufio.NewReader(c), nil)
	if e != nil || r.StatusCode != 200 {
		t.Fatalf("CONNECT %v %v", r, e)
	}
	ca := x509.NewCertPool()
	ca.AddCert(s.Certificate())
	tc := tls.Client(c, &tls.Config{RootCAs: ca, ServerName: "example.com", MinVersion: tls.VersionTLS12})
	if e := tc.Handshake(); e != nil {
		t.Fatal(e)
	}
	fmt.Fprint(tc, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	r, e = http.ReadResponse(bufio.NewReader(tc), nil)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(r.Body)
	if e != nil || string(b) != "encrypted-body" || count.Load() != 1 {
		t.Fatalf("TLS %q %v", b, e)
	}
	if _, e := p.Close(); e != nil {
		t.Fatal(e)
	}
}
func TestProxyRejectsDestinations(t *testing.T) {
	for _, target := range []string{"other.example:80", "user:secret@example.com:80", "example.com.evil:80", "example.com%2fevil:80"} {
		t.Run(target, func(t *testing.T) {
			p, path := testProxy(t, context.Background(), "example.com:80", newProxyBudget())
			c := proxyConn(t, path)
			fmt.Fprintf(c, "GET http://%s/secret HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
			failedProxy(t, p)
			a, e := p.Close()
			if e == nil || len(a) != 1 || strings.Contains(a[0].Value, "secret") || strings.Contains(e.Error(), "secret") {
				t.Fatalf("unsafe failure %#v %v", a, e)
			}
		})
	}
}
func TestProxyConflictingAuthority(t *testing.T) {
	var count atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count.Add(1) }))
	defer s.Close()
	target := strings.TrimPrefix(s.URL, "http://")
	p, path := testProxy(t, context.Background(), target, newProxyBudget())
	c := proxyConn(t, path)
	fmt.Fprintf(c, "GET %s/ HTTP/1.1\r\nHost: forbidden.example\r\n\r\n", s.URL)
	failedProxy(t, p)
	if count.Load() != 0 {
		t.Fatal("conflicting Host reached upstream")
	}
}
func TestProxyBufferedCONNECT(t *testing.T) {
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	go func() {
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	p, path := testProxy(t, context.Background(), ln.Addr().String(), newProxyBudget())
	c := proxyConn(t, path)
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nearly-data", ln.Addr(), ln.Addr())
	br := bufio.NewReader(c)
	r, e := http.ReadResponse(br, nil)
	if e != nil || r.StatusCode != 200 {
		t.Fatal(e)
	}
	b := make([]byte, 10)
	if _, e := io.ReadFull(br, b); e != nil || string(b) != "early-data" {
		t.Fatalf("buffered %q %v", b, e)
	}
	p.Close()
}
func TestProxyRedirectDoesNotFollow(t *testing.T) {
	var forbidden atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forbidden.Add(1) }))
	defer other.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer s.Close()
	_, path := testProxy(t, context.Background(), strings.TrimPrefix(s.URL, "http://"), newProxyBudget())
	r, _ := proxyRequest(t, path, strings.TrimPrefix(s.URL, "http://"))
	if r.StatusCode != 302 || forbidden.Load() != 0 {
		t.Fatal("redirect followed")
	}
}
func TestProxyCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, path := testProxy(t, ctx, "example.com:80", newProxyBudget())
	c := proxyConn(t, path)
	io.WriteString(c, "GET ")
	cancel()
	failedProxy(t, p)
	if _, e := c.Read(make([]byte, 1)); e == nil {
		t.Fatal("connection left open")
	}
}
func TestProxyPhaseBudgetsAndAudit(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer s.Close()
	b := newProxyBudget()
	target := strings.TrimPrefix(s.URL, "http://")
	for i := 0; i < 2; i++ {
		p, path := testProxy(t, context.Background(), target, b)
		proxyRequest(t, path, target)
		a, e := p.Close()
		if e != nil || len(a) != 1 {
			t.Fatalf("phase audit %#v %v", a, e)
		}
		a[0].Value = "modified"
		again, _ := p.Close()
		if again[0].Value != target {
			t.Fatal("Close leaked mutable audit")
		}
	}
}
func TestProxyStartPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing")
	if e := os.WriteFile(path, []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := startProxy(context.Background(), path, collect.Invocation{Phase: "source", HostPort: "example.com:80"}, newProxyBudget()); e == nil {
		t.Fatal("existing path accepted")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "keep" {
		t.Fatal("existing path modified")
	}
	if _, e := startProxy(context.Background(), filepath.Join(t.TempDir(), "join"), collect.Invocation{Phase: "join", HostPort: "example.com:80"}, newProxyBudget()); e == nil {
		t.Fatal("join accepted")
	}
}

func TestProxyExpectContinue(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Expect") != "" {
			t.Error("Expect reached upstream")
		}
		b, e := io.ReadAll(r.Body)
		if e != nil || string(b) != "withheld-body" {
			t.Errorf("request body %q: %v", b, e)
		}
		io.WriteString(w, "accepted-body")
	}))
	t.Cleanup(s.Close)
	target := strings.TrimPrefix(s.URL, "http://")
	p, path := testProxy(t, context.Background(), target, newProxyBudget())
	c := proxyConn(t, path)
	br := bufio.NewReader(c)
	fmt.Fprintf(c, "POST http://%s/ HTTP/1.1\r\nHost: %s\r\nExpect: 100-continue\r\nContent-Length: 13\r\n\r\n", target, target)
	interim, e := http.ReadResponse(br, nil)
	if e != nil {
		t.Fatal(e)
	}
	if interim.StatusCode != 100 {
		t.Fatalf("status before body %d", interim.StatusCode)
	}
	io.WriteString(c, "withheld-body")
	r, e := http.ReadResponse(br, nil)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(r.Body)
	if e != nil || string(b) != "accepted-body" || requests.Load() != 1 {
		t.Fatalf("response %q %v, requests %d", b, e, requests.Load())
	}
	if _, e := p.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestProxyInformationalResponses(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		c, _, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		defer c.Close()
		io.WriteString(c, "HTTP/1.1 100 Continue\r\nConnection: close, X-Remove\r\nX-Remove: secret\r\nProxy-Authorization: private\r\n\r\nHTTP/1.1 102 Processing\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: </style.css>; rel=preload\r\nConnection: close, X-Remove\r\nX-Remove: secret\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nfinal-body")
	}))
	t.Cleanup(s.Close)
	target := strings.TrimPrefix(s.URL, "http://")
	p, path := testProxy(t, context.Background(), target, newProxyBudget())
	c := proxyConn(t, path)
	br := bufio.NewReader(c)
	fmt.Fprintf(c, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	for _, status := range []int{100, 102, 103, 200} {
		r, e := http.ReadResponse(br, nil)
		if e != nil {
			t.Fatal(e)
		}
		if r.StatusCode != status {
			t.Fatalf("status %d, want %d", r.StatusCode, status)
		}
		if r.Header.Get("X-Remove") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Fatal("informational hop headers leaked")
		}
		if status == 103 && r.Header.Get("Link") != "</style.css>; rel=preload" {
			t.Fatal("early hints missing")
		}
		if status == 200 {
			b, e := io.ReadAll(r.Body)
			if e != nil || string(b) != "final-body" {
				t.Fatalf("response body %q %v", b, e)
			}
		}
	}
	a, e := p.Close()
	if e != nil || len(a) != 1 || requests.Load() != 1 {
		t.Fatalf("audit %#v %v, requests %d", a, e, requests.Load())
	}
}

func TestProxyUnsupportedExpectation(t *testing.T) {
	for _, expect := range []string{"Expect: private-unsupported\r\n", "Expect: 100-continue, other\r\n", "Expect: 100-continue\r\nExpect: 100-continue\r\n"} {
		t.Run(expect, func(t *testing.T) {
			var requests atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.Copy(io.Discard, r.Body) }))
			t.Cleanup(s.Close)
			target := strings.TrimPrefix(s.URL, "http://")
			p, path := testProxy(t, context.Background(), target, newProxyBudget())
			c := proxyConn(t, path)
			fmt.Fprintf(c, "POST http://%s/ HTTP/1.1\r\nHost: %s\r\n%sContent-Length: 3\r\n\r\n", target, target, expect)
			failedProxy(t, p)
			if requests.Load() != 0 {
				t.Fatal("unsupported expectation reached upstream")
			}
			if strings.Contains(p.Err().Error(), "private") {
				t.Fatal("error disclosed expectation")
			}
		})
	}
}

func TestProxyInterimCancellation(t *testing.T) {
	closed := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		defer c.Close()
		defer close(closed)
		io.WriteString(c, "HTTP/1.1 103 Early Hints\r\nLink: </first.css>\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: ")
		io.Copy(io.Discard, c)
	}))
	t.Cleanup(s.Close)
	target := strings.TrimPrefix(s.URL, "http://")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, path := testProxy(t, ctx, target, newProxyBudget())
	c := proxyConn(t, path)
	br := bufio.NewReader(c)
	fmt.Fprintf(c, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	r, e := http.ReadResponse(br, nil)
	if e != nil || r.StatusCode != 103 {
		t.Fatalf("interim %#v %v", r, e)
	}
	cancel()
	failedProxy(t, p)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upstream interim stream left open")
	}
	if _, e := br.ReadByte(); e == nil {
		t.Fatal("inbound interim stream left open")
	}
}

func TestProxyTunnelCopyFailure(t *testing.T) {
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release); ln.Close() })
	go func() {
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		<-release
	}()
	p, path := testProxy(t, context.Background(), ln.Addr().String(), newProxyBudget())
	c := proxyConn(t, path)
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr(), ln.Addr())
	if _, e := http.ReadResponse(bufio.NewReader(c), nil); e != nil {
		t.Fatal(e)
	}
	// An actual read deadline faults only the child-to-upstream copy while
	// the upstream-to-child copy remains blocked on a live, idle TCP peer.
	p.mu.Lock()
	var inbound net.Conn
	for socket, child := range p.sockets {
		if child {
			inbound = socket
			break
		}
	}
	p.mu.Unlock()
	if inbound == nil {
		t.Fatal("missing accepted socket")
	}
	if e := inbound.SetReadDeadline(time.Now().Add(-time.Second)); e != nil {
		t.Fatal(e)
	}
	failedProxy(t, p)
	audit, e := p.Close()
	if e != errProxyForward || len(audit) != 2 || audit[1].Permission != "network-error" || audit[1].Value != ln.Addr().String() {
		t.Fatalf("copy failure %#v %v", audit, e)
	}
	again, e := p.Close()
	if e != errProxyForward || len(again) != 2 {
		t.Fatalf("sticky copy failure %#v %v", again, e)
	}
}

func TestProxyTunnelUpstreamHalfClose(t *testing.T) {
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ln.Close() })
	received := make(chan string, 1)
	go func() {
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(c, "upstream-finished")
		c.(*net.TCPConn).CloseWrite()
		body, _ := io.ReadAll(c)
		received <- string(body)
	}()
	p, path := testProxy(t, context.Background(), ln.Addr().String(), newProxyBudget())
	c := proxyConn(t, path)
	br := bufio.NewReader(c)
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr(), ln.Addr())
	if _, e := http.ReadResponse(br, nil); e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(br)
	if e != nil || string(body) != "upstream-finished" {
		t.Fatalf("half-close response %q %v", body, e)
	}
	if _, e := io.WriteString(c, "client-after-upstream-eof"); e != nil {
		t.Fatal(e)
	}
	if e := c.(*net.UnixConn).CloseWrite(); e != nil {
		t.Fatal(e)
	}
	select {
	case body := <-received:
		if body != "client-after-upstream-eof" {
			t.Fatalf("truncated reverse traffic %q", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reverse traffic did not complete")
	}
	waitFor(t, func() bool { p.budget.mu.Lock(); defer p.budget.mu.Unlock(); return p.budget.active == 0 })
	if _, e := p.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestProxyLongTemporaryPath(t *testing.T) {
	longRoot := filepath.Join(t.TempDir(), strings.Repeat("long-root-", 12))
	if err := os.MkdirAll(longRoot, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", longRoot)
	t.Setenv("GOTMPDIR", longRoot)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "portable-proxy") }))
	t.Cleanup(s.Close)
	target := strings.TrimPrefix(s.URL, "http://")
	var socketDir string
	t.Run(strings.Repeat("long-subtest-", 8), func(t *testing.T) {
		p, path := testProxy(t, context.Background(), target, newProxyBudget())
		socketDir = filepath.Dir(path)
		info, err := os.Stat(socketDir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("socket directory permissions %o", info.Mode().Perm())
		}
		_, body := proxyRequest(t, path, target)
		if body != "portable-proxy" {
			t.Fatalf("response body %q", body)
		}
		if _, err := p.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if socketDir != "" {
		if _, err := os.Stat(socketDir); !os.IsNotExist(err) {
			t.Fatalf("socket directory remains after cleanup: %v", err)
		}
	}
}
