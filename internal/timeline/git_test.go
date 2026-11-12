package timeline

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
)

type gitRunner struct {
	run    func(context.Context, string, ...string) executil.Result
	stream func(context.Context, io.Writer, string, ...string) executil.Result
}

func (r gitRunner) Run(ctx context.Context, name string, args ...string) executil.Result {
	return r.run(ctx, name, args...)
}
func (r gitRunner) Stream(ctx context.Context, w io.Writer, name string, args ...string) executil.Result {
	return r.stream(ctx, w, name, args...)
}

func localGit(t *testing.T, repo string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), append([]string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Timeline Test", "GIT_AUTHOR_EMAIL=timeline@example.invalid", "GIT_COMMITTER_NAME=Timeline Test", "GIT_COMMITTER_EMAIL=timeline@example.invalid"}, env...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitRecordsAndFailures(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo with spaces")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	localGit(t, repo, nil, "init", "-b", "main")
	scope := timelineScope()
	scope.Repo = repo
	t.Run("unborn", func(t *testing.T) {
		r := CollectGit(context.Background(), scope, executil.Runner{})
		if len(r.Entries) != 0 || len(r.Errors) != 0 || r.Summary.Status != "ok" {
			t.Fatalf("result=%+v", r)
		}
	})
	commit := func(date, subject string) string {
		localGit(t, repo, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", subject)
		return localGit(t, repo, nil, "rev-parse", "HEAD")
	}
	first := commit("2026-11-11T10:15:00Z", `quoted "subject" 日本語`)
	commit("2026-11-10T09:00:00Z", "skewed parent outside window")
	last := commit("2026-11-11T11:15:00Z", "last subject")
	t.Run("real skewed history and arguments", func(t *testing.T) {
		calls := 0
		real := executil.Runner{}
		runner := gitRunner{run: func(ctx context.Context, name string, args ...string) executil.Result {
			calls++
			return real.Run(ctx, name, args...)
		}, stream: func(ctx context.Context, w io.Writer, name string, args ...string) executil.Result {
			calls++
			want := []string{"--no-pager", "-C", repo, "log", "-z", "--no-show-signature", "--no-decorate", "--no-color", "--since-as-filter=2026-11-11T10:00:00Z", "--until=2026-11-11T12:00:00Z", "--format=%H%x00%cI%x00%s"}
			if name != "git" || !reflect.DeepEqual(args, want) {
				t.Errorf("command=%s %q", name, args)
			}
			return real.Stream(ctx, w, name, args...)
		}}
		r := CollectGit(context.Background(), scope, runner)
		if len(r.Errors) != 0 || len(r.Entries) != 2 || calls > 5 {
			t.Fatalf("calls=%d result=%+v", calls, r)
		}
		for i, want := range []struct{ sha, stamp, subject string }{{last, "2026-11-11T11:15:00Z", "last subject"}, {first, "2026-11-11T10:15:00Z", `quoted "subject" 日本語`}} {
			e := r.Entries[i]
			if e.ID != want.sha || e.Time.Format(time.RFC3339) != want.stamp || e.Details["subject"] != want.subject || e.TimeBasis != "committer-time" {
				t.Errorf("entry=%+v", e)
			}
		}
	})
	t.Run("shallow", func(t *testing.T) {
		clone := filepath.Join(t.TempDir(), "shallow")
		localGit(t, repo, nil, "clone", "--depth=1", "file://"+repo, clone)
		s := scope
		s.Repo = clone
		r := CollectGit(context.Background(), s, executil.Runner{})
		if len(r.Entries) != 1 || len(r.Errors) != 0 || !hasGap(r, "history") {
			t.Fatalf("result=%+v", r)
		}
	})
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		s := scope
		s.Repo = path
		r := CollectGit(context.Background(), s, executil.Runner{})
		if len(r.Errors) == 0 || !hasGap(r, "missing") {
			t.Fatalf("path=%s result=%+v", path, r)
		}
	}
	t.Run("absent repo", func(t *testing.T) {
		s := scope
		s.Repo = ""
		r := CollectGit(context.Background(), s, nil)
		if len(r.Errors) != 0 || !hasGap(r, "missing") {
			t.Fatalf("result=%+v", r)
		}
	})
	t.Run("missing tooling", func(t *testing.T) {
		r := CollectGit(context.Background(), scope, gitRunner{run: func(context.Context, string, ...string) executil.Result {
			return executil.Result{Err: exec.ErrNotFound}
		}})
		if len(r.Errors) == 0 || !hasGap(r, "missing") {
			t.Fatalf("result=%+v", r)
		}
	})
	for _, tc := range []struct {
		name, body string
		err        error
		stderr     string
	}{
		{"chunks", strings.Repeat("a", 40) + "\x002026-11-11T11:00:00Z\x00token=hidden 日本語\x00", nil, ""},
		{"truncated", strings.Repeat("a", 40) + "\x002026-11-11T11:00:00Z\x00unterminated", nil, ""},
		{"unsupported filter", "", errors.New("exit status 129"), "fatal: unrecognized argument: --since-as-filter=... token=hidden"},
		{"timeout", "", context.DeadlineExceeded, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := gitRunner{run: func(_ context.Context, _ string, args ...string) executil.Result {
				if args[len(args)-1] == "--is-shallow-repository" {
					return executil.Result{Stdout: "false\n"}
				}
				return executil.Result{Stdout: strings.Repeat("a", 40)}
			}, stream: func(_ context.Context, w io.Writer, _ string, _ ...string) executil.Result {
				for _, b := range []byte(tc.body) {
					if _, err := w.Write([]byte{b}); err != nil {
						return executil.Result{Err: err}
					}
				}
				return executil.Result{Err: tc.err, Stderr: tc.stderr}
			}}
			r := CollectGit(context.Background(), scope, runner)
			if tc.name == "chunks" {
				if len(r.Entries) != 1 || strings.Contains(r.Entries[0].Change, "hidden") {
					t.Fatalf("result=%+v", r)
				}
			} else if len(r.Errors) == 0 || len(r.Entries) != 0 {
				t.Fatalf("result=%+v", r)
			}
			if strings.Contains(strings.Join(r.Errors, " "), "hidden") || strings.Contains(strings.Join(r.Errors, " "), "BEGIN EXTERNAL") {
				t.Fatalf("unsafe diagnostics=%v", r.Errors)
			}
		})
	}
}

func TestGitRecordsLargeFieldDeadline(t *testing.T) {
	scope := timelineScope()
	scope.Repo = "repository"
	body := strings.Repeat("a", 40) + "\x002026-11-11T11:00:00Z\x00" + strings.Repeat("x", 6<<20) + " token=hidden\x00"
	runner := gitRunner{run: func(_ context.Context, _ string, args ...string) executil.Result {
		if args[len(args)-1] == "--is-shallow-repository" {
			return executil.Result{Stdout: "false"}
		}
		return executil.Result{Stdout: strings.Repeat("a", 40)}
	}, stream: func(_ context.Context, w io.Writer, _ string, _ ...string) executil.Result {
		_, err := io.WriteString(w, body)
		return executil.Result{Err: err}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := CollectGit(ctx, scope, runner)
	t.Logf("6 MiB git field elapsed: %s", time.Since(start))
	if r.Summary.Status != "timeout" || len(r.Entries) != 0 || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("elapsed=%s result=%+v", time.Since(start), r)
	}
}
