package coderuntime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/collect"
)

func TestProxyLimits(t *testing.T) {
	t.Run("requests across phases", func(t *testing.T) {
		var count atomic.Int32
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count.Add(1); io.WriteString(w, "ok") }))
		defer s.Close()
		target := strings.TrimPrefix(s.URL, "http://")
		b := newProxyBudget()
		for phase := 0; phase < 2; phase++ {
			p, path := testProxy(t, context.Background(), target, b)
			for i := 0; i < 32; i++ {
				proxyRequest(t, path, target)
			}
			if _, e := p.Close(); e != nil {
				t.Fatal(e)
			}
		}
		p, path := testProxy(t, context.Background(), target, b)
		c := proxyConn(t, path)
		fmt.Fprintf(c, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		failedProxy(t, p)
		if count.Load() != 64 {
			t.Fatalf("upstream requests %d", count.Load())
		}
	})
	t.Run("active connections", func(t *testing.T) {
		p, path := testProxy(t, context.Background(), "example.com:80", newProxyBudget())
		for i := 0; i < 8; i++ {
			c := proxyConn(t, path)
			io.WriteString(c, "GET ")
		}
		waitFor(t, func() bool { p.budget.mu.Lock(); defer p.budget.mu.Unlock(); return p.budget.active == 8 })
		if p.Err() != nil {
			t.Fatal("eight connections failed")
		}
		proxyConn(t, path)
		failedProxy(t, p)
	})
	t.Run("header boundary", func(t *testing.T) {
		for _, size := range []int{16 << 10, (16 << 10) + 1} {
			t.Run(fmt.Sprint(size), func(t *testing.T) {
				var count atomic.Int32
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count.Add(1); io.WriteString(w, "ok") }))
				defer s.Close()
				target := strings.TrimPrefix(s.URL, "http://")
				p, path := testProxy(t, context.Background(), target, newProxyBudget())
				c := proxyConn(t, path)
				prefix := fmt.Sprintf("GET http://%s/ HTTP/1.1\r\nHost: %s\r\nX-Pad: ", target, target)
				request := prefix + strings.Repeat("a", size-len(prefix)-4) + "\r\n\r\n"
				io.WriteString(c, request)
				if size == 16<<10 {
					r, e := http.ReadResponse(bufio.NewReader(c), nil)
					if e != nil {
						t.Fatal(e)
					}
					io.Copy(io.Discard, r.Body)
					if count.Load() != 1 {
						t.Fatal("exact header rejected")
					}
				} else {
					failedProxy(t, p)
					if count.Load() != 0 {
						t.Fatal("oversized header reached server")
					}
				}
			})
		}
	})
	t.Run("audit exact boundary", func(t *testing.T) {
		b := newProxyBudget()
		e := collect.AuditEvent{Phase: "source", SourceID: "s", At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Permission: "network-allowed", Value: "example.com:80"}
		raw, _ := json.Marshal([]collect.AuditEvent{e})
		b.auditBytes = (1 << 20) - len(raw) + 2
		if err := b.audit(e); err != nil {
			t.Fatal(err)
		}
		if err := b.audit(e); err == nil {
			t.Fatal("audit overflow accepted")
		}
		p, path := testProxy(t, context.Background(), "example.com:80", b)
		c := proxyConn(t, path)
		io.WriteString(c, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
		failedProxy(t, p)
		a, _ := p.Close()
		if len(a) != 0 {
			t.Fatal("oversized event retained")
		}
	})
	t.Run("upstream header boundary", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				return
			}
			defer c.Close()
			io.WriteString(c, "HTTP/1.1 200 OK\r\nX-Pad: "+strings.Repeat("a", 16<<10)+"\r\nContent-Length: 0\r\n\r\n")
		}))
		defer s.Close()
		target := strings.TrimPrefix(s.URL, "http://")
		p, path := testProxy(t, context.Background(), target, newProxyBudget())
		c := proxyConn(t, path)
		fmt.Fprintf(c, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		failedProxy(t, p)
	})
	t.Run("bad framing", func(t *testing.T) {
		for _, header := range []string{"Content-Length: 1\r\nTransfer-Encoding: chunked\r\n", "Content-Length: 1\r\nContent-Length: 1\r\n", "Upgrade: websocket\r\n", "Host: duplicate.example\r\n"} {
			p, path := testProxy(t, context.Background(), "example.com:80", newProxyBudget())
			c := proxyConn(t, path)
			io.WriteString(c, "POST http://example.com/ HTTP/1.1\r\nHost: example.com\r\n"+header+"\r\n")
			failedProxy(t, p)
		}
	})
	t.Run("connection failure", func(t *testing.T) {
		ln, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		target := ln.Addr().String()
		ln.Close()
		p, path := testProxy(t, context.Background(), target, newProxyBudget())
		c := proxyConn(t, path)
		fmt.Fprintf(c, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		failedProxy(t, p)
		a, _ := p.Close()
		if len(a) != 2 || a[1].Permission != "network-error" || a[1].Value != target {
			t.Fatalf("failure audit %#v", a)
		}
	})
	t.Run("chunked request and response", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, e := io.ReadAll(r.Body)
			if e != nil || string(b) != "hello" {
				t.Errorf("request %q %v", b, e)
			}
			w.(http.Flusher).Flush()
			io.WriteString(w, "streamed")
		}))
		defer s.Close()
		target := strings.TrimPrefix(s.URL, "http://")
		p, path := testProxy(t, context.Background(), target, newProxyBudget())
		c := proxyConn(t, path)
		fmt.Fprintf(c, "POST http://%s/ HTTP/1.1\r\nHost: %s\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n", target, target)
		r, e := http.ReadResponse(bufio.NewReader(c), nil)
		if e != nil {
			t.Fatal(e)
		}
		body, e := io.ReadAll(r.Body)
		if e != nil || string(body) != "streamed" {
			t.Fatalf("response %q %v", body, e)
		}
		if _, e := p.Close(); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("slow body cancellation", func(t *testing.T) {
		seen := make(chan struct{})
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(seen); io.Copy(io.Discard, r.Body) }))
		defer s.Close()
		target := strings.TrimPrefix(s.URL, "http://")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p, path := testProxy(t, ctx, target, newProxyBudget())
		c := proxyConn(t, path)
		fmt.Fprintf(c, "POST http://%s/ HTTP/1.1\r\nHost: %s\r\nContent-Length: 100\r\n\r\nx", target, target)
		select {
		case <-seen:
		case <-time.After(5 * time.Second):
			t.Fatal("body not streaming")
		}
		cancel()
		failedProxy(t, p)
		if _, e := c.Read(make([]byte, 1)); e == nil {
			t.Fatal("body socket open")
		}
	})
	t.Run("tunnel cancellation", func(t *testing.T) {
		ln, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		defer ln.Close()
		closed := make(chan struct{})
		go func() {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			defer c.Close()
			io.Copy(io.Discard, c)
			close(closed)
		}()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p, path := testProxy(t, ctx, ln.Addr().String(), newProxyBudget())
		c := proxyConn(t, path)
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr(), ln.Addr())
		if _, e := http.ReadResponse(bufio.NewReader(c), nil); e != nil {
			t.Fatal(e)
		}
		cancel()
		failedProxy(t, p)
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream open")
		}
	})
	t.Run("sticky and frozen", func(t *testing.T) {
		p, path := testProxy(t, context.Background(), "example.com:80", newProxyBudget())
		c := proxyConn(t, path)
		io.WriteString(c, "GET http://other.example/secret HTTP/1.1\r\nHost: other.example\r\n\r\n")
		failedProxy(t, p)
		first := p.Err()
		a, e := p.Close()
		if e != first || len(a) != 1 || a[0].Value != "other.example:80" {
			t.Fatalf("audit %#v %v", a, e)
		}
		if c, e := net.Dial("unix", path); e == nil {
			c.Close()
			t.Fatal("accepted after failure")
		}
		p.fail(errProxyParse, "network-error", "later")
		again, e := p.Close()
		if e != first || len(again) != 1 {
			t.Fatal("failure not sticky")
		}
	})
}
func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition timeout")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProxyTargets(t *testing.T) {
	for _, tc := range []struct{ method, target, host, want string }{
		{"GET", "http://EXAMPLE.com/path", "example.com:80", "example.com:80"},
		{"GET", "http://example.com:00080/path", "EXAMPLE.com", "example.com:80"},
		{"CONNECT", "example.com", "example.com:443", "example.com:443"},
		{"CONNECT", "[::1]", "[::1]:443", "[::1]:443"},
		{"GET", "http://[0:0:0:0:0:0:0:1]:80/", "[::1]", "[::1]:80"},
		{"GET", "http://user:secret@example.com/", "example.com", ""},
		{"GET", "http://example.com./", "example.com.", ""},
		{"GET", "http://example.com%2fother/", "example.com", ""},
		{"GET", "http://example.com\\other/", "example.com", ""},
		{"GET", "http://example.com/#fragment", "example.com", ""},
		{"GET", "/relative", "example.com", ""},
		{"GET", "https://example.com/", "example.com", ""},
		{"CONNECT", "*.example.com:443", "", ""},
		{"CONNECT", "example.com:0", "", ""},
		{"CONNECT", "example.com:65536", "", ""},
		{"CONNECT", "example.com:+443", "", ""},
		{"CONNECT", "example.com:", "", ""},
		{"CONNECT", "[fe80::1%lo]:443", "", ""},
		{"CONNECT", "[example.com]:443", "", ""},
		{"CONNECT", "[127.0.0.1]:443", "", ""},
		{"CONNECT", "exämple.com:443", "", ""},
		{"CONNECT", "example.com\t:443", "", ""},
		{"CONNECT", "127.1:443", "", ""},
		{"CONNECT", "example.com:443", "other.example:443", ""},
		{"CONNECT", strings.Repeat("a", 254) + ":443", "", ""},
	} {
		t.Run(tc.target, func(t *testing.T) {
			got, e := requestTarget(tc.method, tc.target, tc.host)
			if tc.want == "" {
				if e == nil {
					t.Fatalf("accepted %q", got)
				}
			} else if e != nil || got != tc.want {
				t.Fatalf("got %q %v want %q", got, e, tc.want)
			}
		})
	}
}

func TestProxyByteLimits(t *testing.T) {
	t.Run("real streamed exhaustion", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "33554432")
			chunk := strings.Repeat("x", 32<<10)
			for i := 0; i < 1024; i++ {
				if _, e := io.WriteString(w, chunk); e != nil {
					return
				}
			}
		}))
		defer s.Close()
		target := strings.TrimPrefix(s.URL, "http://")
		b := newProxyBudget()
		p, path := testProxy(t, context.Background(), target, b)
		c := proxyConn(t, path)
		fmt.Fprintf(c, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		n, _ := io.Copy(io.Discard, c)
		failedProxy(t, p)
		b.mu.Lock()
		processed := b.bytes
		b.mu.Unlock()
		if processed != 32<<20 || n >= 32<<20 || n < 31<<20 {
			t.Fatalf("processed=%d received=%d", processed, n)
		}
	})
	t.Run("exact tunnel EOF and next phase", func(t *testing.T) {
		ln, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		defer ln.Close()
		ready := make(chan struct{})
		go func() {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			defer c.Close()
			<-ready
			io.WriteString(c, "abcd")
		}()
		b := newProxyBudget()
		p, path := testProxy(t, context.Background(), ln.Addr().String(), b)
		c := proxyConn(t, path)
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr(), ln.Addr())
		br := bufio.NewReader(c)
		if _, e := http.ReadResponse(br, nil); e != nil {
			t.Fatal(e)
		}
		b.mu.Lock()
		b.bytes = (32 << 20) - 4
		b.mu.Unlock()
		close(ready)
		data, e := io.ReadAll(br)
		if e != nil || string(data) != "abcd" {
			t.Fatalf("exact bytes %q %v", data, e)
		}
		if _, e := p.Close(); e != nil {
			t.Fatal(e)
		}
		p, path = testProxy(t, context.Background(), ln.Addr().String(), b)
		c = proxyConn(t, path)
		io.WriteString(c, "x")
		failedProxy(t, p)
	})
	t.Run("concurrent near cap", func(t *testing.T) {
		ln, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		defer ln.Close()
		ready := make(chan struct{})
		var peers sync.WaitGroup
		peers.Add(2)
		go func() {
			for i := 0; i < 2; i++ {
				c, e := ln.Accept()
				if e != nil {
					return
				}
				go func() { defer c.Close(); peers.Done(); <-ready; io.WriteString(c, "abcd") }()
			}
		}()
		b := newProxyBudget()
		p, path := testProxy(t, context.Background(), ln.Addr().String(), b)
		readers := []*bufio.Reader{}
		for i := 0; i < 2; i++ {
			c := proxyConn(t, path)
			fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr(), ln.Addr())
			br := bufio.NewReader(c)
			if _, e := http.ReadResponse(br, nil); e != nil {
				t.Fatal(e)
			}
			readers = append(readers, br)
		}
		peers.Wait()
		b.mu.Lock()
		b.bytes = (32 << 20) - 8
		b.mu.Unlock()
		close(ready)
		for _, br := range readers {
			data, e := io.ReadAll(br)
			if e != nil || string(data) != "abcd" {
				t.Fatalf("concurrent bytes %q %v", data, e)
			}
		}
		if _, e := p.Close(); e != nil {
			t.Fatal(e)
		}
	})
}

func TestProxyNumericHostnameApproval(t *testing.T) {
	got, e := requestTarget("CONNECT", "0xhost:443", "0xhost:443")
	if e != nil || got != "0xhost:443" {
		t.Fatalf("approved DNS hostname rejected: %q %v", got, e)
	}
}

func TestProxyByteReservationRefund(t *testing.T) {
	b := newProxyBudget()
	b.bytes = (32 << 20) - 4
	n, e := b.reserve(context.Background(), 2)
	if e != nil || n != 2 {
		t.Fatal(n, e)
	}
	result := make(chan error, 1)
	go func() { result <- b.consume(context.Background(), 4) }()
	select {
	case e := <-result:
		t.Fatalf("read failed while write could refund: %v", e)
	case <-time.After(20 * time.Millisecond):
	}
	b.settle(2, 0)
	select {
	case e := <-result:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("reservation not released")
	}
	if b.bytes != 32<<20 {
		t.Fatal("read bytes not accounted")
	}
}

func TestProxyByteLimitsBothDirections(t *testing.T) {
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
		b := make([]byte, 4)
		if _, e := io.ReadFull(c, b); e == nil {
			c.Write(b)
		}
	}()
	b := newProxyBudget()
	p, path := testProxy(t, context.Background(), ln.Addr().String(), b)
	c := proxyConn(t, path)
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr(), ln.Addr())
	br := bufio.NewReader(c)
	if _, e := http.ReadResponse(br, nil); e != nil {
		t.Fatal(e)
	}
	b.mu.Lock()
	b.bytes = (32 << 20) - 8
	b.mu.Unlock()
	io.WriteString(c, "abcd")
	got, e := io.ReadAll(br)
	if e != nil || string(got) != "abcd" {
		t.Fatalf("bidirectional %q %v", got, e)
	}
	if _, e := p.Close(); e != nil {
		t.Fatal(e)
	}
	b.mu.Lock()
	n := b.bytes
	b.mu.Unlock()
	if n != 32<<20 {
		t.Fatalf("processed %d", n)
	}
}

func TestProxyAuditPrefixAndEncoding(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer s.Close()
	target := strings.TrimPrefix(s.URL, "http://")
	b := newProxyBudget()
	all := []collect.AuditEvent{}
	for i := 0; i < 2; i++ {
		p, path := testProxy(t, context.Background(), target, b)
		proxyRequest(t, path, target)
		a, e := p.Close()
		if e != nil {
			t.Fatal(e)
		}
		all = append(all, a...)
	}
	raw, e := json.Marshal(all)
	if e != nil {
		t.Fatal(e)
	}
	b.mu.Lock()
	encoded := b.auditBytes
	b.mu.Unlock()
	if len(raw) != encoded {
		t.Fatalf("audit encoded %d, actual %d", encoded, len(raw))
	}
	p, path := testProxy(t, context.Background(), target, b)
	proxyRequest(t, path, target)
	b.mu.Lock()
	b.auditBytes = 1 << 20
	b.mu.Unlock()
	c := proxyConn(t, path)
	io.WriteString(c, "GET http://denied.example/secret HTTP/1.1\r\nHost: denied.example\r\n\r\n")
	failedProxy(t, p)
	a, e := p.Close()
	if e != errProxyAudit || len(a) != 1 || a[0].Value != target {
		t.Fatalf("prefix %#v %v", a, e)
	}
}

func TestProxyIdleAtByteCap(t *testing.T) {
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
		io.Copy(io.Discard, c)
	}()
	b := newProxyBudget()
	p, path := testProxy(t, context.Background(), ln.Addr().String(), b)
	c := proxyConn(t, path)
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr(), ln.Addr())
	br := bufio.NewReader(c)
	if _, e := http.ReadResponse(br, nil); e != nil {
		t.Fatal(e)
	}
	b.mu.Lock()
	b.bytes = (32 << 20) - 1
	b.mu.Unlock()
	io.WriteString(c, "x")
	waitFor(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.bytes == 32<<20 })
	select {
	case <-p.Failed():
		t.Fatal("idle read invented overflow")
	case <-time.After(20 * time.Millisecond):
	}
	c.(*net.UnixConn).CloseWrite()
	if _, e := io.ReadAll(br); e != nil {
		t.Fatal(e)
	}
	if _, e := p.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestProxyMalformedChunk(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body) }))
	defer s.Close()
	target := strings.TrimPrefix(s.URL, "http://")
	p, path := testProxy(t, context.Background(), target, newProxyBudget())
	c := proxyConn(t, path)
	fmt.Fprintf(c, "POST http://%s/ HTTP/1.1\r\nHost: %s\r\nTransfer-Encoding: chunked\r\n\r\nnot-a-size\r\n", target, target)
	failedProxy(t, p)
}

func TestProxyCloseBlockedPeers(t *testing.T) {
	p, path := testProxy(t, context.Background(), "example.com:80", newProxyBudget())
	c := proxyConn(t, path)
	io.WriteString(c, "GET ")
	waitFor(t, func() bool { p.budget.mu.Lock(); defer p.budget.mu.Unlock(); return p.budget.active == 1 })
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close waited for peer")
	}
	if _, e := c.Read(make([]byte, 1)); e == nil {
		t.Fatal("Close left inbound open")
	}
}

func TestProxyInformationalLimits(t *testing.T) {
	for _, wire := range []string{
		"HTTP/1.1 101 Switching Protocols\r\n\r\n",
		"HTTP/1.1 103 Early Hints\r\nUpgrade: websocket\r\n\r\n",
		"HTTP/1.1 103 Early Hints\r\nContent-Length: 1\r\n\r\nx",
		"HTTP/1.1 103 Early Hints\r\nX-Pad: " + strings.Repeat("a", 16<<10) + "\r\n\r\n",
	} {
		t.Run(fmt.Sprint(len(wire)), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _, e := w.(http.Hijacker).Hijack()
				if e != nil {
					return
				}
				defer c.Close()
				io.WriteString(c, wire)
			}))
			t.Cleanup(s.Close)
			target := strings.TrimPrefix(s.URL, "http://")
			p, path := testProxy(t, context.Background(), target, newProxyBudget())
			c := proxyConn(t, path)
			fmt.Fprintf(c, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
			data, _ := io.ReadAll(c)
			failedProxy(t, p)
			if len(data) != 0 {
				t.Fatalf("forwarded invalid interim response %q", data)
			}
		})
	}
	t.Run("shared byte allowance", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				return
			}
			defer c.Close()
			io.WriteString(c, strings.Repeat("HTTP/1.1 103 Early Hints\r\n\r\n", 10))
		}))
		t.Cleanup(s.Close)
		target := strings.TrimPrefix(s.URL, "http://")
		request := fmt.Sprintf("GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		b := newProxyBudget()
		b.bytes = (32 << 20) - len(request) - 100
		p, path := testProxy(t, context.Background(), target, b)
		c := proxyConn(t, path)
		io.WriteString(c, request)
		data, _ := io.ReadAll(c)
		failedProxy(t, p)
		if len(data) != 100 {
			t.Fatalf("interim allowance received %d bytes, want100", len(data))
		}
		a, e := p.Close()
		if e != errProxyLimit || a[len(a)-1].Value != "bytes" {
			t.Fatalf("interim failure %#v %v", a, e)
		}
	})
}

func TestProxyHTTP10TransferEncoding(t *testing.T) {
	for _, encoding := range []string{"chunked", "identity", ""} {
		t.Run(encoding, func(t *testing.T) {
			var requests atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.WriteString(w, "unexpected-forward") }))
			t.Cleanup(s.Close)
			target := strings.TrimPrefix(s.URL, "http://")
			p, path := testProxy(t, context.Background(), target, newProxyBudget())
			c := proxyConn(t, path)
			fmt.Fprintf(c, "POST http://%s/ HTTP/1.0\r\nHost: %s\r\nTransfer-Encoding: %s\r\n\r\n4\r\ndata\r\n0\r\n\r\n", target, target, encoding)
			data, _ := io.ReadAll(c)
			if len(data) != 0 || requests.Load() != 0 {
				t.Fatalf("invalid framing forwarded: response %q, upstream requests %d", data, requests.Load())
			}
			failedProxy(t, p)
			if p.Err() != errProxyParse {
				t.Fatalf("framing error %v", p.Err())
			}
		})
	}
	t.Run("content length remains supported", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, e := io.ReadAll(r.Body)
			if e != nil || string(body) != "data" {
				t.Errorf("body %q %v", body, e)
			}
			io.WriteString(w, "ok")
		}))
		t.Cleanup(s.Close)
		target := strings.TrimPrefix(s.URL, "http://")
		p, path := testProxy(t, context.Background(), target, newProxyBudget())
		c := proxyConn(t, path)
		fmt.Fprintf(c, "POST http://%s/ HTTP/1.0\r\nHost: %s\r\nContent-Length: 4\r\n\r\ndata", target, target)
		response, e := http.ReadResponse(bufio.NewReader(c), nil)
		if e != nil {
			t.Fatal(e)
		}
		body, e := io.ReadAll(response.Body)
		if e != nil || string(body) != "ok" {
			t.Fatalf("response %q %v", body, e)
		}
		if _, e := p.Close(); e != nil {
			t.Fatal(e)
		}
	})
}
