package executil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStreamDeliversBeforeProcessExit(t *testing.T) {
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Result, 1)
	go func() {
		result := (Runner{Timeout: time.Second}).Stream(ctx, writer, "sh", "-c", "printf 'first\\n'; sleep 10")
		writer.Close()
		done <- result
	}()
	first := make([]byte, 6)
	if _, err := io.ReadFull(reader, first); err != nil || string(first) != "first\n" {
		t.Fatalf("stream: %q %v", first, err)
	}
	cancel()
	reader.Close()
	select {
	case result := <-done:
		if !errors.Is(result.Err, context.Canceled) {
			t.Fatalf("%+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("producer did not stop")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("consumer failed") }

func TestStreamConsumerFailureStopsProducer(t *testing.T) {
	start := time.Now()
	result := (Runner{Timeout: 2 * time.Second}).Stream(context.Background(), brokenWriter{}, "sh", "-c", "printf 'line\\n'; sleep 10")
	if result.Err == nil || time.Since(start) > time.Second {
		t.Fatalf("consumer failure: %+v after %s", result, time.Since(start))
	}
}

func TestStreamProducerFailureAndDeadline(t *testing.T) {
	var out bytes.Buffer
	result := (Runner{}).Stream(context.Background(), &out, "sh", "-c", "printf 'line\\n'; echo 'password=hidden' >&2; exit 7")
	if result.Err == nil || result.ExitCode == nil || *result.ExitCode != 7 || out.String() != "line\n" || strings.Contains(result.Stderr, "hidden") {
		t.Fatalf("%+v stdout=%q", result, out.String())
	}
	start := time.Now()
	result = (Runner{Timeout: 30 * time.Millisecond}).Stream(context.Background(), io.Discard, "sh", "-c", "sleep 10 & wait")
	if !errors.Is(result.Err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("deadline: %+v", result)
	}
}
