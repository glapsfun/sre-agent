package coderuntime

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/glapsfun/sre-agent/internal/collect"
)

var errProxyParse = errors.New("invalid proxy message")
var errProxyForward = errors.New("proxy forwarding failed")

type egressProxy struct {
	mu                      sync.Mutex
	ctx                     context.Context
	cancel                  context.CancelFunc
	listener                net.Listener
	budget                  *proxyBudget
	phase, sourceID, target string
	failed                  chan struct{}
	err                     error
	closed                  bool
	sockets                 map[net.Conn]bool
	audit                   []collect.AuditEvent
}

func startProxy(ctx context.Context, socketPath string, inv collect.Invocation, budget *proxyBudget) (*egressProxy, error) {
	target, err := normalizeAuthority(inv.HostPort, "")
	if err != nil || inv.Phase != "source" || budget == nil {
		return nil, errors.New("invalid proxy invocation")
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		return nil, errors.New("proxy socket unavailable")
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, errors.New("proxy listen failed")
	}
	child, cancel := context.WithCancel(ctx)
	p := &egressProxy{ctx: child, cancel: cancel, listener: listener, budget: budget, phase: inv.Phase, sourceID: inv.SourceID, target: target, failed: make(chan struct{}), sockets: make(map[net.Conn]bool), audit: []collect.AuditEvent{}}
	go p.accept()
	go func() { <-child.Done(); p.fail(errors.New("proxy canceled"), "network-error", "canceled") }()
	return p, nil
}
func (p *egressProxy) Failed() <-chan struct{} { return p.failed }
func (p *egressProxy) Err() error              { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (p *egressProxy) shutdownLocked() {
	p.closed = true
	p.cancel()
	p.listener.Close()
	for c, inbound := range p.sockets {
		c.Close()
		if inbound {
			p.budget.release()
		}
		delete(p.sockets, c)
	}
}
func (p *egressProxy) Close() ([]collect.AuditEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.shutdownLocked()
	}
	return append([]collect.AuditEvent{}, p.audit...), p.err
}
func (p *egressProxy) auditLocked(category, value string) error {
	event := collect.AuditEvent{Phase: p.phase, SourceID: p.sourceID, At: time.Now().UTC(), Permission: category, Value: value}
	if err := p.budget.audit(event); err != nil {
		return err
	}
	p.audit = append(p.audit, event)
	return nil
}
func (p *egressProxy) fail(err error, category, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	if auditErr := p.auditLocked(category, value); auditErr != nil {
		err = auditErr
	}
	p.err = err
	close(p.failed)
	p.shutdownLocked()
}
func (p *egressProxy) allowed(target string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	if err := p.auditLocked("network-allowed", target); err != nil {
		p.err = err
		close(p.failed)
		p.shutdownLocked()
		return false
	}
	return true
}
func (p *egressProxy) track(c net.Conn, inbound bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		c.Close()
		return false
	}
	p.sockets[c] = inbound
	return true
}
func (p *egressProxy) untrack(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if inbound, ok := p.sockets[c]; ok {
		c.Close()
		delete(p.sockets, c)
		if inbound {
			p.budget.release()
		}
	}
}
func (p *egressProxy) accept() {
	for {
		c, err := p.listener.Accept()
		if err != nil {
			if p.ctx.Err() == nil {
				p.fail(errProxyForward, "network-error", "accept-failed")
			}
			return
		}
		if !p.budget.acquire() {
			c.Close()
			p.fail(errProxyLimit, "network-limit", "connections")
			return
		}
		if !p.track(c, true) {
			p.budget.release()
			return
		}
		go p.serve(c)
	}
}

type proxyTransport struct {
	net.Conn
	p *egressProxy
}

func (c *proxyTransport) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	// Bounded read-ahead distinguishes a clean EOF at the cap from extra input.
	// Bytes enter the processed allowance before any parser or forwarder sees them.
	c.p.budget.mu.Lock()
	remaining := proxyMaxBytes - c.p.budget.bytes
	c.p.budget.mu.Unlock()
	n, err := c.Conn.Read(data[:min(len(data), proxyCopyBuffer, max(remaining, 1))])
	if n == 0 {
		return n, err
	}
	if consumeErr := c.p.budget.consume(c.p.ctx, n); consumeErr != nil {
		if errors.Is(consumeErr, errProxyLimit) {
			c.p.fail(errProxyLimit, "network-limit", "bytes")
		}
		return 0, consumeErr
	}
	return n, err
}
func (c *proxyTransport) Write(data []byte) (int, error) {
	total := 0
	for len(data) > 0 {
		reserved, err := c.p.budget.reserve(c.p.ctx, min(len(data), proxyCopyBuffer))
		if err != nil {
			if errors.Is(err, errProxyLimit) {
				c.p.fail(errProxyLimit, "network-limit", "bytes")
			}
			return total, err
		}
		n, err := c.Conn.Write(data[:reserved])
		c.p.budget.settle(reserved, n)
		total += n
		data = data[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

// Read the complete bounded header before allowing net/http to normalize Host.
func proxyHeader(br *bufio.Reader) ([]byte, string, error) {
	header := make([]byte, 0, proxyMaxHeader)
	host := ""
	hosts, cl, te := 0, 0, 0
	allowsTransferEncoding := false
	for {
		line, err := br.ReadSlice('\n')
		if len(header)+len(line) > proxyMaxHeader {
			return nil, "", errProxyLimit
		}
		header = append(header, line...)
		if err != nil {
			return nil, "", errProxyParse
		}
		if len(line) < 2 || line[len(line)-2] != '\r' {
			return nil, "", errProxyParse
		}
		if len(header) == len(line) {
			startLine := string(line[:len(line)-2])
			version, _, _ := strings.Cut(startLine, " ")
			if !strings.HasPrefix(startLine, "HTTP/") {
				if space := strings.LastIndexByte(startLine, ' '); space >= 0 {
					version = startLine[space+1:]
				}
			}
			major, minor, ok := http.ParseHTTPVersion(version)
			allowsTransferEncoding = ok && (major > 1 || major == 1 && minor >= 1)
			continue
		}
		if len(line) == 2 {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			return nil, "", errProxyParse
		}
		name, value, ok := strings.Cut(string(line[:len(line)-2]), ":")
		if !ok {
			return nil, "", errProxyParse
		}
		switch strings.ToLower(name) {
		case "host":
			hosts++
			host = strings.TrimSpace(value)
			if host == "" {
				return nil, "", errProxyParse
			}
		case "content-length":
			cl++
		case "transfer-encoding":
			te++
		}
	}
	// net/http discards Transfer-Encoding on HTTP/1.0, so reject it first.
	if hosts > 1 || cl > 1 || te > 1 || cl > 0 && te > 0 || te > 0 && !allowsTransferEncoding {
		return nil, "", errProxyParse
	}
	return header, host, nil
}
func stripProxyHeaders(h http.Header, raw []byte) {
	// net/http removes a response's Connection header when it contains close,
	// so nominated fields must be recovered from the bounded original header.
	for len(raw) > 0 {
		line, rest, _ := bytes.Cut(raw, []byte("\r\n"))
		raw = rest
		name, value, ok := strings.Cut(string(line), ":")
		if ok && strings.EqualFold(name, "Connection") {
			for _, key := range strings.Split(value, ",") {
				h.Del(strings.TrimSpace(key))
			}
		}
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
}
func (p *egressProxy) serve(raw net.Conn) {
	defer p.untrack(raw)
	c := &proxyTransport{Conn: raw, p: p}
	br := bufio.NewReaderSize(c, proxyMaxHeader)
	header, host, err := proxyHeader(br)
	if err != nil {
		p.messageFailure(err, "invalid-target")
		return
	}
	parsed := bufio.NewReaderSize(io.MultiReader(bytes.NewReader(header), br), proxyMaxHeader)
	req, err := http.ReadRequest(parsed)
	if err != nil {
		p.messageFailure(errProxyParse, "invalid-target")
		return
	}
	if !p.budget.request() {
		p.fail(errProxyLimit, "network-limit", "requests")
		return
	}
	target, err := requestTarget(req.Method, req.RequestURI, host)
	if err != nil {
		p.fail(errTarget, "network-denied", "invalid-target")
		return
	}
	if target != p.target {
		p.fail(errTarget, "network-denied", target)
		return
	}
	if req.Header.Get("Upgrade") != "" || req.Method == "CONNECT" && (req.ContentLength != 0 || len(req.TransferEncoding) > 0) {
		p.messageFailure(errProxyParse, target)
		return
	}
	expectations := req.Header.Values("Expect")
	if len(expectations) > 0 && (len(expectations) != 1 || !strings.EqualFold(strings.TrimSpace(expectations[0]), "100-continue") || req.Method == "CONNECT" || req.ProtoMajor != 1 || req.ProtoMinor != 1) {
		p.messageFailure(errProxyParse, target)
		return
	}
	if !p.allowed(target) {
		return
	}
	upstream, err := (&net.Dialer{}).DialContext(p.ctx, "tcp", p.target)
	if err != nil {
		p.fail(errProxyForward, "network-error", target)
		return
	}
	if !p.track(upstream, false) {
		return
	}
	defer p.untrack(upstream)
	if req.Method == "CONNECT" {
		p.tunnel(c, parsed, upstream, target)
		return
	}
	if len(expectations) > 0 {
		if _, err := io.WriteString(c, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			p.fail(errProxyForward, "network-error", target)
			return
		}
	}
	req.Header.Del("Expect")
	stripProxyHeaders(req.Header, header)
	req.Host = p.target
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.RequestURI = ""
	req.Close = true
	req.Trailer = nil
	if err := req.Write(upstream); err != nil {
		p.fail(errProxyForward, "network-error", target)
		return
	}
	responseReader := bufio.NewReaderSize(upstream, proxyMaxHeader)
	for {
		responseHeader, _, err := proxyHeader(responseReader)
		if err != nil {
			p.messageFailure(err, target)
			return
		}
		// Parse interim headers in isolation so the next response stays in the
		// original bounded reader, without accumulating nested reader chains.
		response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(responseHeader)), req)
		if err != nil || response.Header.Get("Upgrade") != "" {
			p.messageFailure(errProxyParse, target)
			return
		}
		if response.StatusCode < 200 {
			if (response.StatusCode != 100 && response.StatusCode != 102 && response.StatusCode != 103) || response.Header.Get("Content-Length") != "" || len(response.TransferEncoding) > 0 {
				p.messageFailure(errProxyParse, target)
				return
			}
			stripProxyHeaders(response.Header, responseHeader)
			response.Close = false
			if err := response.Write(c); err != nil {
				p.fail(errProxyForward, "network-error", target)
				return
			}
			continue
		}
		// Attach the streaming body only for the final response.
		response, err = http.ReadResponse(bufio.NewReaderSize(io.MultiReader(bytes.NewReader(responseHeader), responseReader), proxyMaxHeader), req)
		if err != nil {
			p.messageFailure(errProxyParse, target)
			return
		}
		stripProxyHeaders(response.Header, responseHeader)
		response.Close = true
		response.Trailer = nil
		if err := response.Write(c); err != nil {
			p.fail(errProxyForward, "network-error", target)
		}
		return
	}
}

func (p *egressProxy) messageFailure(err error, target string) {
	if errors.Is(err, errProxyLimit) {
		p.fail(errProxyLimit, "network-limit", "headers")
	} else {
		p.fail(errProxyParse, "network-error", target)
	}
}
func (p *egressProxy) tunnel(c *proxyTransport, br *bufio.Reader, upstream net.Conn, target string) {
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		p.fail(errProxyForward, "network-error", target)
		return
	}
	finish := func(err error, destination net.Conn) {
		if err == nil {
			if half, ok := destination.(interface{ CloseWrite() error }); ok {
				err = half.CloseWrite()
			} else {
				err = errProxyForward
			}
		}
		if err != nil {
			// fail ignores socket errors caused by intentional phase shutdown.
			p.fail(errProxyForward, "network-error", target)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := io.CopyBuffer(upstream, br, make([]byte, proxyCopyBuffer))
		finish(err, upstream)
	}()
	_, err := io.CopyBuffer(c, upstream, make([]byte, proxyCopyBuffer))
	finish(err, c.Conn)
	// A clean EOF closes only the destination's write half. The reverse copy
	// remains live until its own EOF or explicit phase shutdown.
	select {
	case <-done:
	case <-p.ctx.Done():
	}
}
