package coderuntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/collect"
	"github.com/glapsfun/sre-agent/internal/sources"
)

type pipeChild struct {
	in          *bufio.Reader
	out, stderr *os.File
}

func pipeProtocol(t *testing.T, ctx context.Context, b *protocolBudget, body func(pipeChild) error, helper collect.Helper, invocations ...collect.Invocation) ([]collect.Value, error) {
	t.Helper()
	return pipeProtocolWithWriter(t, ctx, b, body, helper, nil, invocations...)
}

func pipeProtocolWithWriter(t *testing.T, ctx context.Context, b *protocolBudget, body func(pipeChild) error, helper collect.Helper, wrap func(io.WriteCloser) io.WriteCloser, invocations ...collect.Invocation) ([]collect.Value, error) {
	t.Helper()
	inR, inW, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	outR, outW, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	errR, errW, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	p := &sandboxProcess{Stdin: inW, Stdout: outR, Stderr: errR, Done: done}
	if wrap != nil {
		p.Stdin = wrap(inW)
	}
	t.Cleanup(func() { p.Abort(); inR.Close(); outW.Close(); errW.Close() })
	go func() {
		err := body(pipeChild{bufio.NewReader(inR), outW, errW})
		outW.Close()
		errW.Close()
		inR.Close()
		p.mu.Lock()
		p.waitErr = err
		p.mu.Unlock()
		close(done)
	}()
	inv := collect.Invocation{Phase: "source", SourceID: "s", Keys: []collect.Key{{Key: "own", SourceID: "s", Tool: "read"}}}
	if len(invocations) > 0 {
		inv = invocations[0]
	}
	values, err := runProtocol(ctx, p, b, inv, helper, func(context.Context) error { return nil }, nil, nil)
	p.Abort()
	return values, err
}
func fixtureReady(c pipeChild) error {
	if _, e := io.WriteString(c.out, "{\"type\":\"ready\"}\n"); e != nil {
		return e
	}
	_, e := c.in.ReadString('\n')
	return e
}
func TestProtocolPipeSequences(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		exit, ok   bool
	}{
		{"valid", `{"type":"final","values":[]}`, false, true},
		{"missing-final", "", false, false},
		{"duplicate-ready", `{"type":"ready"}`, false, false},
		{"duplicate-final", "{\"type\":\"final\",\"values\":[]}\n{\"type\":\"final\",\"values\":[]}", false, false},
		{"extra-output", "{\"type\":\"final\",\"values\":[]}\nconsole", false, false},
		{"nonzero-exit", `{"type":"final","values":[]}`, true, false},
		{"unknown-key", `{"type":"call","id":"a","key":"other"}`, false, false},
		{"forged-audit", `{"type":"final","values":[],"audit":[{"permission":"network-allowed"}]}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var calls atomic.Int32
			_, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error {
				if err := fixtureReady(c); err != nil {
					return err
				}
				if tc.body != "" {
					io.WriteString(c.out, tc.body+"\n")
				}
				if tc.exit {
					return errors.New("secret exit detail")
				}
				return nil
			}, func(string) (sources.CallResult, error) { calls.Add(1); return sources.CallResult{}, nil })
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("unauthorized helper reached source")
			}
		})
	}
}
func TestProtocolPipeHelperReplies(t *testing.T) {
	for _, helperFails := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		values, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error {
			if err := fixtureReady(c); err != nil {
				return err
			}
			io.WriteString(c.out, "{\"type\":\"call\",\"id\":\"a\",\"key\":\"own\"}\n")
			line, e := c.in.ReadBytes('\n')
			if e != nil {
				return e
			}
			var reply struct {
				ID     string             `json:"id"`
				OK     bool               `json:"ok"`
				Result sources.CallResult `json:"result"`
			}
			if json.Unmarshal(line, &reply) != nil || reply.ID != "a" || reply.OK == helperFails || string(reply.Result.Data) != `{"value":7}` {
				return errors.New("bad helper reply")
			}
			_, e = io.WriteString(c.out, "{\"type\":\"final\",\"values\":[]}\n")
			return e
		}, func(string) (sources.CallResult, error) {
			var e error
			if helperFails {
				e = errors.New("secret detail")
			}
			return sources.CallResult{Data: json.RawMessage(`{"value":7}`)}, e
		})
		cancel()
		if err != nil || len(values) != 0 {
			t.Fatalf("values=%v err=%v", values, err)
		}
	}
}
func TestProtocolPendingAndLateHelper(t *testing.T) {
	for _, body := range []string{
		"{\"type\":\"call\",\"id\":\"b\",\"key\":\"own\"}\n{\"type\":\"call\",\"id\":\"c\",\"key\":\"own\"}\n",
		"{\"type\":\"call\",\"id\":\"a\",\"key\":\"own\"}\n",
		"{\"type\":\"final\",\"values\":[]}\n",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		release := make(chan struct{})
		completed := make(chan struct{}, 2)
		_, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error {
			if err := fixtureReady(c); err != nil {
				return err
			}
			io.WriteString(c.out, "{\"type\":\"call\",\"id\":\"a\",\"key\":\"own\"}\n"+body)
			<-ctx.Done()
			return nil
		}, func(string) (sources.CallResult, error) {
			<-release
			completed <- struct{}{}
			return sources.CallResult{Data: json.RawMessage(`true`)}, nil
		})
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("pending call was not promptly rejected: %v", err)
		}
		close(release)
		cancel()
	}
}
func TestProtocolOutputAndStderrLimits(t *testing.T) {
	for _, stderr := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error {
			if e := fixtureReady(c); e != nil {
				return e
			}
			var w io.Writer = c.out
			if stderr {
				w = c.stderr
			}
			io.WriteString(w, strings.Repeat("x", 70000))
			<-ctx.Done()
			return nil
		}, nil)
		cancel()
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("overflow not rejected: %v", err)
		}
	}
}
func TestProtocolCanceledBlockedInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error { io.WriteString(c.out, "{\"type\":\"ready\"}\n"); <-ctx.Done(); return nil }, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}
func TestCanceledProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	err := New().Probe(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, collect.ErrCodeUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestProtocolBlockedReplyOverflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error {
		if e := fixtureReady(c); e != nil {
			return e
		}
		io.WriteString(c.out, "{\"type\":\"call\",\"id\":\"a\",\"key\":\"own\"}\n")
		if _, e := c.in.Peek(1); e != nil {
			return e
		}
		io.WriteString(c.out, "{\"type\":\"final\",\"values\":[]}\n")
		io.WriteString(c.stderr, strings.Repeat("x", 65537))
		<-ctx.Done()
		return nil
	}, func(string) (sources.CallResult, error) {
		return sources.CallResult{Data: json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)}, nil
	})
	if err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("overflow blocked behind reply: %v (%s)", err, time.Since(start))
	}
}

func TestInvocationValidation(t *testing.T) {
	helper := func(string) (sources.CallResult, error) { return sources.CallResult{}, nil }
	valid := collect.Invocation{Phase: "source", SourceID: "s", HostPort: "example.com:443", Keys: []collect.Key{{Key: "own", SourceID: "s", Tool: "read"}}, Program: strings.Repeat(" ", 65536)}
	if err := validateInvocation(valid, helper); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*collect.Invocation){
		func(i *collect.Invocation) { i.Program += " " }, func(i *collect.Invocation) { i.Program = "\xff" }, func(i *collect.Invocation) { i.SourceID = "other" },
		func(i *collect.Invocation) { i.HostPort = "example.com:443@evil.test" }, func(i *collect.Invocation) { i.Keys = append(i.Keys, i.Keys[0]) },
		func(i *collect.Invocation) { i.Values = []collect.Value{{Key: "other"}} }, func(i *collect.Invocation) { i.Phase = "join" },
	} {
		inv := valid
		inv.Keys = append([]collect.Key{}, valid.Keys...)
		change(&inv)
		if validateInvocation(inv, helper) == nil {
			t.Fatalf("accepted invalid invocation %+v", inv)
		}
	}
	if validateInvocation(valid, nil) == nil {
		t.Fatal("source without helper accepted")
	}
	if validateInvocation(collect.Invocation{Phase: "join"}, helper) == nil {
		t.Fatal("join with helper accepted")
	}
}

func TestProtocolSharedPhaseBudgets(t *testing.T) {
	b := &protocolBudget{frames: 126}
	for phase := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := pipeProtocol(t, ctx, b, func(c pipeChild) error {
			if e := fixtureReady(c); e != nil {
				return e
			}
			_, e := io.WriteString(c.out, "{\"type\":\"final\",\"values\":[]}\n")
			return e
		}, nil)
		cancel()
		if (err == nil) != (phase == 0) {
			t.Fatalf("phase %d err=%v", phase, err)
		}
	}
	for _, tc := range []struct {
		host, stderr int
		overflow     bool
	}{{host: 32 << 20, overflow: true}, {stderr: 65535}, {stderr: 65536, overflow: true}} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := pipeProtocol(t, ctx, &protocolBudget{host: tc.host, stderr: tc.stderr}, func(c pipeChild) error {
			if e := fixtureReady(c); e != nil {
				return e
			}
			io.WriteString(c.stderr, "x")
			_, e := io.WriteString(c.out, "{\"type\":\"final\",\"values\":[]}\n")
			return e
		}, nil)
		cancel()
		if (err != nil) != tc.overflow {
			t.Fatalf("quota %+v err=%v", tc, err)
		}
	}
}

func TestProtocolHelperReplyBound(t *testing.T) {
	base := `{"id":"a","ok":true,"result":{"source_id":"","tool":"","reference":"","at":"0001-01-01T00:00:00Z","data":"","gaps":null}}` + "\n"
	for _, extra := range []int{0, 1} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error {
			if e := fixtureReady(c); e != nil {
				return e
			}
			io.WriteString(c.out, "{\"type\":\"call\",\"id\":\"a\",\"key\":\"own\"}\n")
			line, e := c.in.ReadString('\n')
			if e != nil {
				return e
			}
			if len(line) != 2<<20 {
				return errors.New("incorrect reply bytes")
			}
			_, e = io.WriteString(c.out, "{\"type\":\"final\",\"values\":[]}\n")
			return e
		}, func(string) (sources.CallResult, error) {
			return sources.CallResult{Data: json.RawMessage(`"` + strings.Repeat("x", (2<<20)-len(base)+extra) + `"`)}, nil
		})
		cancel()
		if (err == nil) != (extra == 0) {
			t.Fatalf("extra=%d err=%v", extra, err)
		}
	}
}

func TestProtocolMissingReadyAndUnterminatedFinal(t *testing.T) {
	for _, ready := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error {
			if ready {
				if e := fixtureReady(c); e != nil {
					return e
				}
			}
			line := `{"type":"final","values":[]}`
			if !ready {
				line += "\n"
			}
			_, e := io.WriteString(c.out, line)
			return e
		}, nil)
		cancel()
		if err == nil {
			t.Fatalf("ready=%v accepted incomplete sequence", ready)
		}
	}
}

func TestProtocolReusedCompletedID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var calls atomic.Int32
	_, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error {
		if e := fixtureReady(c); e != nil {
			return e
		}
		call := "{\"type\":\"call\",\"id\":\"a\",\"key\":\"own\"}\n"
		io.WriteString(c.out, call)
		if _, e := c.in.ReadString('\n'); e != nil {
			return e
		}
		io.WriteString(c.out, call)
		<-ctx.Done()
		return nil
	}, func(string) (sources.CallResult, error) { calls.Add(1); return sources.CallResult{}, nil })
	if err == nil || calls.Load() != 1 {
		t.Fatalf("completed ID reused: err=%v calls=%d", err, calls.Load())
	}
}

func TestProtocolLateResultDiscarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, entered, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	b := &protocolBudget{}
	_, err := pipeProtocol(t, ctx, b, func(c pipeChild) error {
		if e := fixtureReady(c); e != nil {
			return e
		}
		io.WriteString(c.out, "{\"type\":\"call\",\"id\":\"a\",\"key\":\"own\"}\n")
		<-entered
		io.WriteString(c.out, "{\"type\":\"final\",\"values\":[]}\n")
		<-ctx.Done()
		return nil
	}, func(string) (sources.CallResult, error) {
		close(entered)
		<-release
		defer close(returned)
		return sources.CallResult{Data: json.RawMessage(`"` + strings.Repeat("x", 3<<20) + `"`)}, nil
	})
	if err == nil {
		t.Fatal("pending final accepted")
	}
	b.mu.Lock()
	before := b.host
	b.mu.Unlock()
	close(release)
	<-returned
	b.mu.Lock()
	after := b.host
	b.mu.Unlock()
	if before != after {
		t.Fatal("late callback wrote host bytes")
	}
}

func TestProbeDeadlineArithmetic(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		remaining, reserve time.Duration
		reject             bool
	}{{-1, 0, true}, {0, 0, true}, {100 * time.Millisecond, 0, true}, {100*time.Millisecond + 2, 50*time.Millisecond + 1, false}, {time.Second, 500 * time.Millisecond, false}, {4 * time.Second, 2 * time.Second, false}, {60 * time.Second, 2 * time.Second, false}} {
		deadline := now.Add(tc.remaining)
		execution, reserve, err := probeDeadlines(now, deadline)
		if (err != nil) != tc.reject {
			t.Fatalf("remaining=%s err=%v", tc.remaining, err)
		}
		if !tc.reject && (reserve != tc.reserve || !execution.Equal(deadline.Add(-tc.reserve))) {
			t.Fatalf("remaining=%s execution=%v reserve=%s", tc.remaining, execution, reserve)
		}
	}
}

func TestProbeShortAndExpiredBudgetsCreateNoState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	for _, duration := range []time.Duration{-time.Second, 100 * time.Millisecond} {
		ctx, cancel := context.WithTimeout(context.Background(), duration)
		err := New().Probe(ctx)
		cancel()
		if !errors.Is(err, collect.ErrCodeUnavailable) {
			t.Fatalf("err=%v", err)
		}
		if duration < 0 && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lost deadline: %v", err)
		}
		entries, e := os.ReadDir(root)
		if e != nil || len(entries) != 0 {
			t.Fatalf("short probe created state: %v %v", entries, e)
		}
	}
}

func TestProtocolCancellationDuringBlockedReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error {
		if e := fixtureReady(c); e != nil {
			return e
		}
		io.WriteString(c.out, "{\"type\":\"call\",\"id\":\"a\",\"key\":\"own\"}\n")
		if _, e := c.in.Peek(1); e != nil {
			return e
		}
		cancel()
		return nil
	}, func(string) (sources.CallResult, error) {
		return sources.CallResult{Data: json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked reply lost cancellation: %v", err)
	}
}

func TestProtocolInitialInvocationBound(t *testing.T) {
	base := `{"phase":"join","source_id":"","keys":[],"values":[{"key":"own","source_id":"s","data":""}]}` + "\n"
	for _, extra := range []int{0, 1} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		inv := collect.Invocation{Phase: "join", Values: []collect.Value{{Key: "own", SourceID: "s", Data: json.RawMessage(`"` + strings.Repeat("x", 65536-len(base)+extra) + `"`)}}}
		_, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error {
			io.WriteString(c.out, "{\"type\":\"ready\"}\n")
			line, e := c.in.ReadString('\n')
			if e != nil {
				return e
			}
			if len(line) != 65536 {
				return errors.New("incorrect initial bytes")
			}
			_, e = io.WriteString(c.out, "{\"type\":\"final\",\"values\":[]}\n")
			return e
		}, nil, inv)
		cancel()
		if (err == nil) != (extra == 0) {
			t.Fatalf("initial extra=%d err=%v", extra, err)
		}
	}
}

func TestProtocolFinalBehindBlockedReply(t *testing.T) {
	for _, extraFrame := range []bool{false, true} {
		t.Run(map[bool]string{false: "unresolved-second-helper", true: "post-final-frame"}[extraFrame], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			secondEntered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			var calls atomic.Int32
			payload := json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)
			start := time.Now()
			_, err := pipeProtocol(t, ctx, &protocolBudget{}, func(c pipeChild) error {
				if e := fixtureReady(c); e != nil {
					return e
				}
				io.WriteString(c.out, "{\"type\":\"call\",\"id\":\"a\",\"key\":\"own\"}\n")
				if !extraFrame {
					io.WriteString(c.out, "{\"type\":\"call\",\"id\":\"b\",\"key\":\"own\"}\n")
					<-secondEntered
				}
				if _, e := c.in.Peek(1); e != nil {
					return e
				}
				io.WriteString(c.out, "{\"type\":\"final\",\"values\":[]}\n")
				if extraFrame {
					io.WriteString(c.out, "{\"type\":\"ready\"}\n")
				}
				<-ctx.Done()
				return nil
			}, func(string) (sources.CallResult, error) {
				if calls.Add(1) == 1 {
					if !extraFrame {
						<-secondEntered
					}
					return sources.CallResult{Data: payload}, nil
				}
				close(secondEntered)
				<-release
				return sources.CallResult{Data: json.RawMessage(`true`)}, nil
			})
			if err == nil || ctx.Err() != nil || time.Since(start) > time.Second {
				t.Fatalf("invalid final waited behind delivery: err=%v elapsed=%s caller=%v", err, time.Since(start), ctx.Err())
			}
		})
	}
}

type delayedReplyWriter struct {
	io.WriteCloser
	returned chan<- struct{}
	release  <-chan struct{}
	delayed  bool
}

func (w *delayedReplyWriter) Write(data []byte) (int, error) {
	n, err := w.WriteCloser.Write(data)
	if !w.delayed && strings.HasPrefix(string(data), `{"id":`) {
		w.delayed = true
		close(w.returned)
		<-w.release
	}
	return n, err
}

func TestProtocolFinalBeforeReplyWriteReturns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	written, finalSent, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := pipeProtocolWithWriter(t, ctx, &protocolBudget{}, func(c pipeChild) error {
			if e := fixtureReady(c); e != nil {
				return e
			}
			io.WriteString(c.out, "{\"type\":\"call\",\"id\":\"a\",\"key\":\"own\"}\n")
			if _, e := c.in.ReadString('\n'); e != nil {
				return e
			}
			<-written
			io.WriteString(c.out, "{\"type\":\"final\",\"values\":[]}\n")
			close(finalSent)
			_, err := c.in.ReadByte()
			if err != io.EOF {
				return errors.New("missing final acknowledgment")
			}
			return nil
		}, func(string) (sources.CallResult, error) {
			return sources.CallResult{Data: json.RawMessage(`true`)}, nil
		}, func(w io.WriteCloser) io.WriteCloser {
			return &delayedReplyWriter{WriteCloser: w, returned: written, release: release}
		})
		done <- err
	}()
	select {
	case <-finalSent:
	case <-ctx.Done():
		close(release)
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-done:
		close(release)
		t.Fatalf("final resolved before write completion: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestProtocolNextCallBeforeReplyWriteReturns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	written, nextSent, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	secondEntered, thirdEntered, releaseSecond := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	done := make(chan error, 1)
	inv := collect.Invocation{Phase: "source", SourceID: "s", Keys: []collect.Key{{Key: "own", SourceID: "s", Tool: "read"}, {Key: "second", SourceID: "s", Tool: "read"}}}
	go func() {
		_, err := pipeProtocolWithWriter(t, ctx, &protocolBudget{}, func(c pipeChild) error {
			if e := fixtureReady(c); e != nil {
				return e
			}
			io.WriteString(c.out, "{\"type\":\"call\",\"id\":\"a\",\"key\":\"own\"}\n{\"type\":\"call\",\"id\":\"b\",\"key\":\"second\"}\n")
			if _, e := c.in.ReadString('\n'); e != nil {
				return e
			}
			<-written
			io.WriteString(c.out, "{\"type\":\"call\",\"id\":\"c\",\"key\":\"own\"}\n")
			close(nextSent)
			select {
			case <-thirdEntered:
			case <-ctx.Done():
				return ctx.Err()
			}
			close(releaseSecond)
			for range 2 {
				if _, e := c.in.ReadString('\n'); e != nil {
					return e
				}
			}
			_, err := io.WriteString(c.out, "{\"type\":\"final\",\"values\":[]}\n")
			return err
		}, func(key string) (sources.CallResult, error) {
			n := calls.Add(1)
			if key == "second" {
				close(secondEntered)
				select {
				case <-releaseSecond:
				case <-ctx.Done():
				}
			}
			if key == "own" {
				if n < 3 {
					<-secondEntered
				} else {
					close(thirdEntered)
				}
			}
			return sources.CallResult{Data: json.RawMessage(`true`)}, nil
		}, func(w io.WriteCloser) io.WriteCloser {
			return &delayedReplyWriter{WriteCloser: w, returned: written, release: release}
		}, inv)
		done <- err
	}()
	select {
	case <-nextSent:
	case <-ctx.Done():
		close(release)
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-done:
		close(release)
		t.Fatalf("next call rejected before write completion: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if calls.Load() != 2 {
		close(release)
		t.Fatalf("third callback began before delivery completed: %d", calls.Load())
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d", calls.Load())
	}
}
