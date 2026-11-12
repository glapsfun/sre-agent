package timeline

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
	"github.com/glapsfun/sre-agent/internal/safety"
)

// The coordinator owns the shared budget. Direct collector calls without a
// deadline get a fallback; an existing deadline is never shortened or restarted.
func externalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, executil.DefaultTimeout)
}

func externalFailure(source, code, message string) SourceResult {
	result := newKubeResult(source)
	result.Summary.Status = code
	sourceGap(&result, code, message)
	result.Errors = append(result.Errors, message)
	return result
}

func gitFailure(ctx context.Context, result executil.Result) SourceResult {
	if ctx.Err() != nil || errors.Is(result.Err, context.DeadlineExceeded) || errors.Is(result.Err, context.Canceled) {
		return externalFailure("git", "timeout", "git collection deadline or cancellation reached")
	}
	message := fmt.Sprint(result.Err)
	if result.Stderr != "" {
		message += ": " + result.Stderr
	}
	clean, err := safety.NewRedactor().RedactContext(ctx, message)
	if err != nil {
		clean = "git command failed; diagnostic could not be safely retained"
	}
	code := "error"
	lower := strings.ToLower(message)
	if errors.Is(result.Err, exec.ErrNotFound) || strings.Contains(lower, "executable file not found") || strings.Contains(lower, "not a git repository") || strings.Contains(lower, "cannot change to") || strings.Contains(lower, "no such file or directory") {
		code = "missing"
	}
	return externalFailure("git", code, clean)
}

func CollectGit(ctx context.Context, scope Scope, runner Runner) SourceResult {
	result := newKubeResult("git")
	if scope.Repo == "" {
		result.Summary.Status = "missing"
		sourceGap(&result, "missing", "no git repository was supplied")
		return result
	}
	ctx, cancel := externalContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return gitFailure(ctx, executil.Result{Err: err})
	}
	if runner == nil {
		return externalFailure("git", "missing", "git runner is unavailable")
	}
	run := func(args ...string) executil.Result {
		return runner.Run(ctx, "git", append([]string{"--no-pager", "-C", scope.Repo}, args...)...)
	}
	shallow := run("rev-parse", "--is-shallow-repository")
	if shallow.Err != nil {
		return gitFailure(ctx, shallow)
	}
	switch strings.TrimSpace(shallow.Stdout) {
	case "true":
		sourceGap(&result, "history", "shallow git repository omits earlier HEAD ancestry")
	case "false":
	default:
		return externalFailure("git", "error", "git returned invalid shallow repository status")
	}
	head := run("rev-parse", "--verify", "--quiet", "HEAD")
	if head.Err != nil {
		// Only a symbolic HEAD whose exact branch ref is absent is unborn.
		// Other failures, including broken refs, remain source errors.
		if head.ExitCode != nil && *head.ExitCode == 1 {
			branch := run("symbolic-ref", "--quiet", "HEAD")
			ref := strings.TrimSpace(branch.Stdout)
			if branch.Err == nil && strings.HasPrefix(ref, "refs/heads/") {
				present := run("show-ref", "--verify", "--quiet", ref)
				if present.ExitCode != nil && *present.ExitCode == 1 {
					return finishKubeResult(result)
				}
				if present.Err != nil {
					return gitFailure(ctx, present)
				}
			}
		}
		return gitFailure(ctx, head)
	}
	args := []string{"--no-pager", "-C", scope.Repo, "log", "-z", "--no-show-signature", "--no-decorate", "--no-color", "--since-as-filter=" + scope.Start.UTC().Format(time.RFC3339Nano), "--until=" + scope.End.UTC().Format(time.RFC3339Nano), "--format=%H%x00%cI%x00%s"}
	output := &gitRecords{ctx: ctx, scope: scope, result: &result}
	command := runner.Stream(ctx, output, "git", args...)
	if output.err != nil {
		return gitFailure(ctx, executil.Result{Err: output.err})
	}
	if command.Err != nil {
		return gitFailure(ctx, command)
	}
	if len(output.field) != 0 || output.index != 0 {
		return externalFailure("git", "error", "incomplete NUL-framed git commit record")
	}
	if err := ctx.Err(); err != nil {
		return gitFailure(ctx, executil.Result{Err: err})
	}
	return finishKubeResult(result)
}

type gitRecords struct {
	ctx    context.Context
	scope  Scope
	result *SourceResult
	field  []byte
	fields [3]string
	index  int
	err    error
}

func (r *gitRecords) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		if r.err = r.ctx.Err(); r.err != nil {
			return written, r.err
		}
		end := bytes.IndexByte(data, 0)
		length := len(data)
		if end >= 0 {
			length = end
		}
		if len(r.field)+length > safety.MaxResponseBytes {
			r.err = safety.ErrResponseTooLarge
			return written, r.err
		}
		r.field = append(r.field, data[:length]...)
		written += length
		data = data[length:]
		if end < 0 {
			break
		}
		r.fields[r.index] = string(r.field)
		r.field = r.field[:0]
		r.index++
		written++
		data = data[1:]
		if r.index == len(r.fields) {
			r.index = 0
			if r.err = r.record(); r.err != nil {
				return written, r.err
			}
		}
	}
	return written, nil
}

func (r *gitRecords) record() error {
	sha, stamp, subject := r.fields[0], r.fields[1], r.fields[2]
	if _, err := hex.DecodeString(sha); err != nil || (len(sha) != 40 && len(sha) != 64) {
		return errors.New("invalid NUL-framed git commit identity")
	}
	r.result.Summary.Observed++
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || t.IsZero() {
		sourceGap(r.result, "timestamp", "git commit has no usable committer timestamp")
		return nil
	}
	if !inWindow(r.scope, t) {
		return nil
	}
	r.result.Summary.Matched++
	clean, err := safety.NewRedactor().RedactContext(r.ctx, subject)
	if err != nil {
		return err
	}
	command, err := safety.NewRedactor().RedactContext(r.ctx, "git --no-pager -C "+strconv.Quote(r.scope.Repo)+" show --no-patch --no-show-signature "+sha)
	if err != nil {
		return err
	}
	r.result.Entries = append(r.result.Entries, Entry{ID: sha, Time: t.UTC(), TimeBasis: "committer-time", Source: "git", Object: Object{Kind: "GitCommit", Name: sha}, Change: clean, Command: command, Details: map[string]any{"sha": sha, "subject": clean}})
	return nil
}
