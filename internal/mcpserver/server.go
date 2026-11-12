// Package mcpserver exposes the SRE services over the MCP stdio protocol.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/glapsfun/sre-agent/internal/discover"
	"github.com/glapsfun/sre-agent/internal/logs"
	"github.com/glapsfun/sre-agent/internal/prom"
	"github.com/glapsfun/sre-agent/internal/store"
	"github.com/glapsfun/sre-agent/internal/timeline"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Dependencies supplies an invocation-scoped clock. Nil uses the service clocks.
type Dependencies struct{ Now func() time.Time }

type dependencies struct {
	now      func() time.Time
	discover func(context.Context, discover.Options) (discover.Report, error)
	foldPod  func(context.Context, logs.PodOptions) (logs.Report, error)
	query    func(context.Context, prom.Options) (prom.Report, error)
	collect  func(context.Context, timeline.Options) (timeline.Report, error)
	open     func(context.Context, string) (*store.Store, error)
}

type Server struct {
	sdk      *mcp.Server
	commands map[string]string
}

func New(deps Dependencies) *Server { return newServer(dependencies{now: deps.Now}) }

func (s *Server) Connect(ctx context.Context, transport mcp.Transport, opts *mcp.ServerSessionOptions) (*mcp.ServerSession, error) {
	return s.sdk.Connect(ctx, &resultTransport{Transport: transport, commands: s.commands}, opts)
}

func (s *Server) Run(ctx context.Context, transport mcp.Transport) error {
	return s.sdk.Run(ctx, &resultTransport{Transport: transport, commands: s.commands})
}

func newServer(d dependencies) *Server {
	if d.discover == nil {
		d.discover = discover.Discover
	}
	if d.foldPod == nil {
		d.foldPod = logs.FoldPod
	}
	if d.query == nil {
		d.query = (prom.Service{Now: d.now, Discover: func(ctx context.Context, opts discover.Options) (discover.Report, error) {
			ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
			defer cancel()
			return d.discover(ctx, opts)
		}}).Query
	}
	if d.collect == nil {
		d.collect = func(ctx context.Context, opts timeline.Options) (timeline.Report, error) {
			return timeline.Collect(ctx, opts, timeline.Dependencies{Now: d.now})
		}
	}
	if d.open == nil {
		d.open = func(ctx context.Context, repo string) (*store.Store, error) {
			root, err := store.ResolveRoot(ctx, repo)
			if err != nil {
				return nil, err
			}
			return store.Open(ctx, root)
		}
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "sre", Version: "1"}, nil)
	server := &Server{sdk: s, commands: map[string]string{}}
	registerTools(server, d)
	return server
}

type resultTransport struct {
	mcp.Transport
	commands map[string]string
}

func (t *resultTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &resultConnection{Connection: c, commands: t.commands, pending: map[jsonrpc.ID]string{}}, nil
}

type resultConnection struct {
	mcp.Connection
	commands map[string]string
	mu       sync.Mutex
	pending  map[jsonrpc.ID]string
}

func (c *resultConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	message, err := c.Connection.Read(ctx)
	if req, ok := message.(*jsonrpc.Request); ok && req.IsCall() && req.Method == "tools/call" {
		var params struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(req.Params, &params) == nil {
			if command, ok := c.commands[params.Name]; ok {
				c.mu.Lock()
				c.pending[req.ID] = command
				c.mu.Unlock()
			}
		}
	}
	return message, err
}

func (c *resultConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	if original, ok := message.(*jsonrpc.Response); ok {
		response := *original
		c.mu.Lock()
		command := c.pending[response.ID]
		delete(c.pending, response.ID)
		c.mu.Unlock()
		if response.Error != nil {
			// SDK parameter and lookup errors can echo raw input before any tool
			// middleware runs. Keep protocol codes/IDs while suppressing that text.
			code := int64(jsonrpc.CodeInternalError)
			var wire *jsonrpc.Error
			if errors.As(response.Error, &wire) {
				code = wire.Code
			}
			response.Error = &jsonrpc.Error{Code: code, Message: "MCP request failed"}
		} else if command != "" {
			// This boundary includes SDK-added metadata and resultType, which are
			// appended after tool handlers and receiving middleware return.
			response.Result = boundEncoded(response.Result, command)
		}
		message = &response
	}
	return c.Connection.Write(ctx, message)
}

type outputWriter struct{ io.Writer }

func (outputWriter) Close() error { return nil }

// Run owns a closeable input for the session lifetime so cancellation can
// interrupt blocked reads. Finite injected readers need no close operation.
// The output and diagnostics remain caller-owned, including process stdout.
func Run(ctx context.Context, input io.Reader, output, diagnostics io.Writer) error {
	reader, ok := input.(io.ReadCloser)
	if !ok {
		reader = io.NopCloser(input)
	}
	err := New(Dependencies{}).Run(ctx, &mcp.IOTransport{Reader: reader, Writer: outputWriter{output}})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Transport parsing errors can contain raw peer bytes, so use a fixed
	// diagnostic instead of attempting to interpret arbitrary protocol text.
	fmt.Fprintln(diagnostics, "MCP transport failed")
	return errors.New("MCP transport failed")
}
