package logs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/safety"
)

func fakeKubectl(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return dir
}

func TestFoldPodPinnedArgumentsAndParity(t *testing.T) {
	input := "2026-11-11T12:00:00Z INFO ready\nERROR Authorization: Bearer hidden\n-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----\n"
	dir := fakeKubectl(t, "printf '%s\\n' \"$@\" > \"$CALLS\"\nprintf '%s' \"$LOG_INPUT\"\n")
	t.Setenv("CALLS", filepath.Join(dir, "calls"))
	t.Setenv("LOG_INPUT", input)
	t.Setenv("SRE_KUBE_CONTEXT", "environment")
	report, err := FoldPod(context.Background(), PodOptions{Context: "explicit", Namespace: "payments", Pod: "checkout-abc", Container: "app", Previous: true, Since: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	want, err := Fold(strings.NewReader(input), Options{})
	if err != nil || !reflect.DeepEqual(report, want) {
		t.Fatalf("parity: %+v %+v %v", report, want, err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	expected := "--context\nexplicit\nlogs\n--namespace=payments\n--timestamps=true\n--container=app\n--previous=true\n--since=1h0m0s\npod/checkout-abc\n"
	if string(calls) != expected {
		t.Fatalf("argv=%q want=%q", calls, expected)
	}
}

func TestFoldPodFailureAndValidation(t *testing.T) {
	fakeKubectl(t, "printf 'INFO partial\\n'\nprintf 'password=pod-secret' >&2\nexit 7\n")
	report, err := FoldPod(context.Background(), PodOptions{Context: "test", Namespace: "ns", Pod: "app"})
	if err == nil || report.TotalLines != 0 || strings.Contains(err.Error(), "pod-secret") || !strings.Contains(err.Error(), "collect pod logs: external command failed: exit status 7") {
		t.Fatalf("%+v %v", report, err)
	}
	for _, opts := range []PodOptions{{Pod: "app"}, {Namespace: "ns"}, {Namespace: "ns", Pod: "--help"}, {Namespace: "ns", Pod: "app", Container: "--follow"}, {Namespace: "ns", Pod: "app", Since: -time.Second}} {
		if _, err := FoldPod(context.Background(), opts); err == nil {
			t.Fatalf("accepted %+v", opts)
		}
	}
}

func TestFoldPodOversizedLinePreservesCause(t *testing.T) {
	dir := fakeKubectl(t, "/bin/cat \"$LOG_FILE\"\n/bin/sleep 10\n")
	path := filepath.Join(dir, "logs")
	if err := os.WriteFile(path, []byte("INFO partial\n"+strings.Repeat("x", safety.MaxResponseBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOG_FILE", path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	report, err := FoldPod(ctx, PodOptions{Context: "test", Namespace: "ns", Pod: "app"})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("consumer failure did not stop producer promptly: %s, err=%v", elapsed, err)
	}
	if err == nil || !strings.Contains(err.Error(), "read logs: bufio.Scanner: token too long") {
		t.Fatalf("lost oversized-line cause: %v", err)
	}
	if !reflect.DeepEqual(report, Report{}) {
		t.Fatalf("returned partial report: %+v", report)
	}
}

func TestFoldPodRetainsCallerCancellation(t *testing.T) {
	fakeKubectl(t, "/bin/sleep 10\n")
	for _, want := range []error{context.DeadlineExceeded, context.Canceled} {
		t.Run(want.Error(), func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if want == context.DeadlineExceeded {
				ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
				timer := time.AfterFunc(30*time.Millisecond, cancel)
				defer timer.Stop()
			}
			defer cancel()
			report, err := FoldPod(ctx, PodOptions{Context: "test", Namespace: "ns", Pod: "app"})
			if !errors.Is(err, want) || !reflect.DeepEqual(report, Report{}) {
				t.Fatalf("caller cancellation lost: report=%+v err=%v", report, err)
			}
		})
	}
}
