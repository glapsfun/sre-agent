package executil

import (
	"context"
	"errors"
	"github.com/glapsfun/sre-agent/internal/safety"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEarlyParentExitTerminatesDescendant(t *testing.T) {
	start := time.Now()
	result := (Runner{Timeout: time.Second}).Run(context.Background(), "sh", "-c", "sleep 30 & echo $!")
	pid, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if err != nil {
		t.Fatalf("missing descendant PID: %+v", result)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if result.Err == nil {
		t.Fatal("inherited-pipe wait was silently accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("parent exit did not bound pipe waiting")
	}
	// A killed orphan can remain a zombie until the host's init reaps it.
	deadline := time.Now().Add(time.Second)
	for {
		state, psErr := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if psErr != nil {
			var exit *exec.ExitError
			if errors.As(psErr, &exit) && exit.ExitCode() == 1 {
				break
			}
			t.Fatalf("cannot inspect descendant state: %v", psErr)
		}
		if strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived runner return: %s", pid, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTimeoutIncludesInheritedPipes(t *testing.T) {
	start := time.Now()
	r := (Runner{Timeout: 60 * time.Millisecond}).Run(context.Background(), "sh", "-c", "sleep 4 & wait")
	if !errors.Is(r.Err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", r.Err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout waited for inherited pipes")
	}
}

func TestExecutionAndRedaction(t *testing.T) {
	runner := Runner{Timeout: time.Second, Secrets: []string{"literal-secret"}}
	r := runner.Run(context.Background(), "sh", "-c", `printf '%s\n' "$1"; printf 'password=hunter2 literal-secret\n' >&2; exit 7`, "test", "$(touch forbidden)")
	if r.Err == nil {
		t.Fatal("lost subprocess failure")
	}
	if r.Stdout != "$(touch forbidden)\n" {
		t.Fatalf("argument was not literal: %q", r.Stdout)
	}
	if strings.Contains(r.Stderr+r.Err.Error(), "hunter2") || strings.Contains(r.Stderr+r.Err.Error(), "literal-secret") {
		t.Fatalf("secret leak: %+v", r)
	}
	r = runner.Run(context.Background(), "/missing/literal-secret")
	if r.Err == nil || strings.Contains(r.Err.Error(), "literal-secret") {
		t.Fatalf("unsafe start error: %v", r.Err)
	}
}

func TestKubernetesContextPinning(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1 $2\" = 'config current-context' ]; then echo current-cluster; else printf '%s\\n' \"$@\"; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct{ explicit, env, want string }{{"explicit-cluster", "env-cluster", "explicit-cluster"}, {"", "env-cluster", "env-cluster"}, {"", "", "current-cluster"}} {
		t.Setenv("SRE_KUBE_CONTEXT", tc.env)
		k, err := NewKubernetes(context.Background(), Runner{Timeout: time.Second}, tc.explicit)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("SRE_KUBE_CONTEXT", "changed")
		for range 2 {
			r := k.Run(context.Background(), "get", "pods")
			if r.Err != nil || r.Stdout != "--context\n"+tc.want+"\nget\npods\n" {
				t.Fatalf("not pinned: %+v", r)
			}
		}
		for _, args := range [][]string{{"--context=other", "get", "pods"}, {"get", "pods", "--context", "other"}} {
			if k.Run(context.Background(), args...).Err == nil {
				t.Fatal("accepted context override")
			}
		}
		for _, flag := range []string{"--server=https://elsewhere", "-shttps://elsewhere", "--kubeconfig=/other", "--cluster=other", "--user=other"} {
			if k.Run(context.Background(), flag, "get", "pods").Err == nil {
				t.Fatalf("accepted target override %s", flag)
			}
		}
	}
}

func TestMissingContextFails(t *testing.T) {
	t.Setenv("SRE_KUBE_CONTEXT", "")
	t.Setenv("PATH", t.TempDir())
	if _, err := NewKubernetes(context.Background(), Runner{}, ""); err == nil {
		t.Fatal("silently accepted unavailable context")
	}
}

func TestDeadlineIncludesOutputRedaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "response.json")
	data := `{"padding":"` + strings.Repeat("x", 4*1024*1024) + `","token":"sensitive-value"}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result := (Runner{Timeout: 50 * time.Millisecond}).Run(context.Background(), "cat", path)
	elapsed := time.Since(start)
	t.Logf("4MiB actual executor response, 50ms deadline: %s", elapsed)
	if elapsed > 300*time.Millisecond {
		t.Fatal("executor redaction exceeded overall deadline")
	}
	if !errors.Is(result.Err, context.DeadlineExceeded) {
		t.Fatalf("missing timeout: %v", result.Err)
	}
	if result.Stdout != "" || result.Stderr != "" {
		t.Fatal("returned incomplete redaction output")
	}
}

func TestOversizedOutputFailsWithoutTruncatedEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "response.json")
	data := `{"padding":"` + strings.Repeat("x", safety.MaxResponseBytes) + `"}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result := (Runner{Timeout: time.Second}).Run(context.Background(), "sh", "-c", `cat "$1"; sleep 10`, "test", path)
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("oversized output did not stop the producer")
	}
	if !errors.Is(result.Err, safety.ErrResponseTooLarge) || result.Stdout != "" {
		t.Fatalf("accepted truncated output: %v", result.Err)
	}
}

func TestStructuredOutputDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "response.json")
	input := "[" + strings.Repeat(`{"x":0},`, 900000-1) + `{"x":0}]`
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result := (Runner{Timeout: 100 * time.Millisecond}).Run(context.Background(), "cat", path)
	elapsed := time.Since(start)
	t.Logf("dense executor output: elapsed=%s err=%v", elapsed, result.Err)
	if elapsed > 350*time.Millisecond {
		t.Fatal("dense output redaction exceeded deadline")
	}
	if result.Err == nil || result.Stdout != "" {
		t.Fatal("rejected structured output produced evidence")
	}
}
