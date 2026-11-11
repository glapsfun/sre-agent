package executil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
