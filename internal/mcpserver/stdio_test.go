package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioLifecycle(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "sre")
	build := exec.Command("go", "build", "-o", binary, "./cmd/sre")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	t.Run("SDK concurrent calls and cancellation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		pidPath := filepath.Join(dir, "probe.pid")
		if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\nprintf '%s' \"$$\" > \"$PROBE_PID\"\nexec /bin/sleep 120\n"), 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, binary, "mcp")
		cmd.Env = append(os.Environ(), "PATH="+dir, "SRE_SKIP_CLUSTER=false", "PROBE_PID="+pidPath)
		var diagnostics, wire bytes.Buffer
		cmd.Stderr = &diagnostics
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "lifecycle", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: io.NopCloser(io.TeeReader(output, &wire)), Writer: input}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		listed, err := cs.ListTools(ctx, nil)
		if err != nil || len(listed.Tools) != 8 {
			t.Fatalf("list: %+v %v", listed, err)
		}
		probeCtx, stopProbe := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := cs.CallTool(probeCtx, &mcp.CallToolParams{Name: "discover", Arguments: map[string]any{"context": "pin", "only": []string{"aws"}, "timeout": "2m"}})
			done <- err
		}()
		var pid int
		for pid == 0 {
			raw, _ := os.ReadFile(pidPath)
			pid, _ = strconv.Atoi(string(raw))
			if ctx.Err() != nil {
				t.Fatal("probe never started")
			}
			if pid == 0 {
				time.Sleep(10 * time.Millisecond)
			}
		}
		var workers sync.WaitGroup
		errors := make(chan error, 8)
		for i := 0; i < 8; i++ {
			workers.Go(func() {
				result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "logs_fold", Arguments: map[string]any{"context": "pin", "text": "ERROR request failed"}})
				if err != nil {
					errors <- err
				} else if result.IsError {
					errors <- fmt.Errorf("peer failed: %+v", result)
				}
			})
		}
		workers.Wait()
		close(errors)
		for err := range errors {
			t.Error(err)
		}
		start := time.Now()
		stopProbe()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled client call succeeded")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancellation exceeded deadline")
		}
		for syscall.Kill(pid, 0) == nil {
			if time.Since(start) > 2*time.Second {
				t.Fatal("cancelled probe process survived")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err = cs.ListTools(ctx, nil); err != nil {
			t.Fatalf("peer session unavailable after cancellation: %v", err)
		}
		bad, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "logs_fold", Arguments: json.RawMessage(`{"context":"pin","text":"data","unknown":"token=planted-secret"}`)})
		if err != nil || !bad.IsError {
			t.Fatalf("malformed input accepted: %+v %v", bad, err)
		}
		if err = cs.Close(); err != nil {
			t.Fatal(err)
		}
		if err = cmd.Wait(); err != nil {
			t.Fatalf("shutdown: %v stderr=%s", err, &diagnostics)
		}
		if strings.Contains(wire.String(), "planted-secret") || strings.Contains(diagnostics.String(), "planted-secret") {
			t.Fatal("secret escaped transport")
		}
		lines := bufio.NewScanner(&wire)
		count := 0
		for lines.Scan() {
			var frame map[string]any
			if json.Unmarshal(lines.Bytes(), &frame) != nil || frame["jsonrpc"] != "2.0" {
				t.Fatalf("stdout diagnostic: %s", lines.Text())
			}
			count++
		}
		if lines.Err() != nil || count < 10 {
			t.Fatalf("wire incomplete: lines=%d error=%v", count, lines.Err())
		}
	})
	t.Run("malformed JSON is never a CLI envelope", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "mcp")
		cmd.Stdin = strings.NewReader("{not-json token=planted-secret\n")
		var out, diagnostics bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &diagnostics
		_ = cmd.Run()
		if ctx.Err() != nil {
			t.Fatal("malformed request hung server")
		}
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			if line == "" {
				continue
			}
			var frame map[string]any
			if json.Unmarshal([]byte(line), &frame) != nil || frame["jsonrpc"] != "2.0" {
				t.Fatalf("non-MCP output: %s", line)
			}
		}
		if strings.Contains(out.String()+diagnostics.String(), "planted-secret") {
			t.Fatalf("unsafe diagnostics: %s %s", &out, &diagnostics)
		}
	})
	t.Run("malformed params preserve framing and redact SDK errors", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "mcp")
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var diagnostics bytes.Buffer
		cmd.Stderr = &diagnostics
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(output)
		exchange := func(request string) map[string]any {
			t.Helper()
			if _, err := fmt.Fprintln(input, request); err != nil {
				t.Fatal(err)
			}
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v %s", err, &diagnostics)
			}
			var frame map[string]any
			if err := json.Unmarshal([]byte(line), &frame); err != nil || frame["jsonrpc"] != "2.0" {
				t.Fatalf("invalid protocol output: %s", line)
			}
			if strings.Contains(line, "planted-secret") {
				t.Fatalf("SDK error echoed a secret: %s", line)
			}
			return frame
		}
		init := exchange(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
		if init["result"] == nil {
			t.Fatalf("initialize: %+v", init)
		}
		if _, err := fmt.Fprintln(input, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
			t.Fatal(err)
		}
		bad := exchange(`{"jsonrpc":"2.0","id":"malformed","method":"tools/call","params":{"name":42,"arguments":{"token":"planted-secret"}}}`)
		if bad["id"] != "malformed" || bad["error"].(map[string]any)["code"] != float64(-32602) {
			t.Fatalf("error identity/code changed: %+v", bad)
		}
		good := exchange(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"logs_fold","arguments":{"context":"pin","text":"ERROR failure"}}}`)
		if good["id"] != float64(3) || good["result"] == nil {
			t.Fatalf("session corrupted after malformed request: %+v", good)
		}
		_ = input.Close()
		if err := cmd.Wait(); err != nil {
			t.Fatalf("shutdown: %v %s", err, &diagnostics)
		}
		if strings.Contains(diagnostics.String(), "planted-secret") {
			t.Fatal("secret in stderr")
		}
	})
}
