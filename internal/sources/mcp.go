package sources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errMCP = errors.New("source connection or tool inventory unavailable")

// openSession does not grant approval. Its caller must supply an authorized
// endpoint and credential reference and keep ctx alive until cleanup completes.
func (s Service) openSession(ctx context.Context, source Source) (*mcp.ClientSession, []Tool, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if ValidateSource(source) != nil || source.Kind != "mcp-http" {
		return nil, nil, nil, errMCP
	}
	lookup := s.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	token := ""
	if source.CredentialEnv != "" {
		var ok bool
		token, ok = lookup(source.CredentialEnv)
		if !ok || token == "" || strings.ContainsAny(token, "\r\n") {
			return nil, nil, nil, errMCP
		}
	}
	client := http.Client{}
	if s.HTTPClient != nil {
		client = *s.HTTPClient
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errMCP }
	client.Transport = &sourceTransport{base: base, ctx: ctx, endpoint: source.Endpoint, token: token}
	transport := &mcp.StreamableClientTransport{Endpoint: source.Endpoint, HTTPClient: &client, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: maxRegistryBytes}
	logHandler := &sourceLogHandler{}
	clientOptions := &mcp.ClientOptions{
		Capabilities:   &mcp.ClientCapabilities{},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
		Logger:         slog.New(logHandler),
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "sre-agent-source-scout", Version: "1"}, clientOptions).Connect(ctx, transport, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		return nil, nil, nil, sourceError(ctx)
	}
	cleanup := func() { _ = session.Close() }
	tools, err := listDescriptors(ctx, session, &logHandler.failed)
	if err != nil {
		cleanup()
		return nil, nil, nil, sourceError(ctx)
	}
	return session, tools, cleanup, nil
}

func sourceError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errMCP
}

func listDescriptors(ctx context.Context, session *mcp.ClientSession, invalid *atomic.Bool) ([]Tool, error) {
	tools := []Tool{}
	seen := map[string]bool{}
	cursors := map[string]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		result, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil || result == nil || invalid.Load() {
			return nil, sourceError(ctx)
		}
		for _, tool := range result.Tools {
			if tool == nil || !identifierPattern.MatchString(tool.Name) || !safeText(tool.Name, 128) || seen[tool.Name] || len(tools) >= 100 {
				return nil, errMCP
			}
			schema, ok := tool.InputSchema.(map[string]any)
			if !ok || schema == nil {
				return nil, errMCP
			}
			raw, err := json.Marshal(schema)
			if err != nil || len(raw) > maxRegistryBytes {
				return nil, errMCP
			}
			digest := sha256.Sum256(raw)
			tools = append(tools, Tool{Name: tool.Name, SchemaDigest: hex.EncodeToString(digest[:]), ReadOnly: tool.Annotations != nil && tool.Annotations.ReadOnlyHint})
			seen[tool.Name] = true
		}
		if result.NextCursor == "" {
			sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
			return tools, nil
		}
		cursor = result.NextCursor
		if cursors[cursor] {
			return nil, errMCP
		}
		cursors[cursor] = true
	}
	return nil, errMCP
}

type sourceTransport struct {
	base            http.RoundTripper
	ctx             context.Context
	endpoint, token string
}

func (t *sourceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ctx.Err() != nil {
		return nil, t.ctx.Err()
	}
	if req.URL.String() != t.endpoint || req.Host != "" && req.Host != req.URL.Host {
		return nil, errMCP
	}
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	finish := func() { stop(); cancel() }
	clone := req.Clone(ctx)
	clone.Header.Del("Authorization")
	clone.Header.Del("Cookie")
	// Tool schemas cannot promote sensitive filters into transport headers.
	for name := range clone.Header {
		if strings.HasPrefix(strings.ToLower(name), "mcp-param-") {
			delete(clone.Header, name)
		}
	}
	if t.token != "" {
		clone.Header.Set("Authorization", "Bearer "+t.token)
	}
	if req.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(req.Body, maxRegistryBytes+1))
		_ = req.Body.Close()
		if err != nil || len(raw) > maxRegistryBytes {
			finish()
			return nil, errMCP
		}
		clone.Body = io.NopCloser(bytes.NewReader(raw))
		clone.ContentLength = int64(len(raw))
		clone.GetBody = nil
	}
	resp, err := t.base.RoundTrip(clone)
	if err != nil {
		finish()
		return nil, errMCP
	}
	if resp == nil || resp.Body == nil {
		finish()
		return nil, errMCP
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 || resp.ContentLength > maxRegistryBytes {
		resp.Body.Close()
		finish()
		return nil, errMCP
	}
	resp.Body = &boundedSourceBody{ReadCloser: resp.Body, remaining: maxRegistryBytes, finish: finish}
	return resp, nil
}

type boundedSourceBody struct {
	io.ReadCloser
	remaining int
	finish    func()
}

func (b *boundedSourceBody) Read(p []byte) (int, error) {
	if len(p) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	if n > b.remaining {
		b.remaining = 0
		return 0, errMCP
	}
	b.remaining -= n
	return n, err
}
func (b *boundedSourceBody) Close() error { err := b.ReadCloser.Close(); b.finish(); return err }

// The SDK drops malformed descriptors and logs errors instead of returning them.
// Remember only that validation failed, never untrusted diagnostic content.
type sourceLogHandler struct{ failed atomic.Bool }

func (h *sourceLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *sourceLogHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Level >= slog.LevelError {
		h.failed.Store(true)
	}
	return nil
}
func (h *sourceLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *sourceLogHandler) WithGroup(string) slog.Handler      { return h }
