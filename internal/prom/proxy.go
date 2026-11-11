package prom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
)

// Reject every method except GET. Go regular expressions do not support a
// negative lookahead; rejecting just standard write methods would admit others.
const rejectNonGET = `^($|[^G].*|G$|G[^E].*|GE$|GE[^T].*|GET.+)$`

func readProxy(ctx context.Context, kube *executil.Kubernetes, path string, params url.Values) (body []byte, err error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Unix socket paths are short on both supported platforms. A long TMPDIR
	// (common on macOS and in test worktrees) can exceed the kernel path limit.
	dir, err := os.MkdirTemp("/tmp", "sre-p-")
	if err != nil {
		return nil, errors.New("cannot create private proxy directory; provide --url")
	}
	defer func() {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil && err == nil {
			body = nil
			err = errors.New("cannot remove private proxy directory")
		}
	}()
	socket := filepath.Join(dir, "p.sock")
	processCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		kube.Stream(processCtx, io.Discard, "proxy", "--unix-socket="+socket, "--accept-hosts=^localhost$", "--accept-paths=^"+regexp.QuoteMeta(path)+"$", "--reject-methods="+rejectNonGET, "--disable-filter=false")
	}()
	defer func() { stop(); <-done }()

	requestCtx, cancelRequest := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-done:
			cancelRequest()
		case <-requestCtx.Done():
		}
	}()
	defer func() { cancelRequest(); <-watched }()
	if err := waitProxy(requestCtx, socket); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("kubectl proxy failed to start; provide --url")
	}
	dialer := net.Dialer{}
	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != "localhost:80" {
				return nil, errors.New("unexpected proxy destination")
			}
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	body, err = (Service{Client: client}).httpRead(requestCtx, "http://localhost"+path+"?"+params.Encode())
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	select {
	case <-done:
		return nil, errors.New("kubectl proxy exited before query completed; provide --url")
	default:
	}
	if err != nil {
		return nil, fmt.Errorf("Kubernetes service proxy: %w; provide --url for a reachable endpoint", err)
	}
	return body, nil
}

func waitProxy(ctx context.Context, socket string) error {
	dialer := net.Dialer{}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := dialer.DialContext(ctx, "unix", socket)
		if err == nil {
			return connection.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
