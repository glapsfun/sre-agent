package timeline

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
	"github.com/glapsfun/sre-agent/internal/safety"
)

func Validate(opts Options) error {
	if !objectName.MatchString(opts.Namespace) {
		return errors.New("valid namespace is required")
	}
	if opts.Since < 0 || opts.Timeout < 0 {
		return errors.New("since and timeout must be positive durations")
	}
	if opts.Workload != "" {
		parts := strings.Split(opts.Workload, "/")
		if len(parts) > 2 {
			return errors.New("workload must be NAME or KIND/NAME")
		}
		if len(parts) == 2 && workloadKinds[strings.ToLower(parts[0])] == "" {
			return errors.New("unsupported workload kind")
		}
		if !objectName.MatchString(parts[len(parts)-1]) {
			return errors.New("invalid workload name")
		}
	}
	return nil
}

type pinnedKube struct {
	KubeRunner
	selected string
}

func (k pinnedKube) Context() string { return k.selected }

func Collect(ctx context.Context, opts Options, deps Dependencies) (Report, error) {
	if err := Validate(opts); err != nil {
		return Report{}, err
	}
	if opts.Since == 0 {
		opts.Since = 2 * time.Hour
	}
	if opts.Timeout == 0 {
		opts.Timeout = executil.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	end := now().UTC()
	if opts.AlertmanagerURL == "" {
		opts.AlertmanagerURL = os.Getenv("SRE_ALERTMANAGER_URL")
	}
	runner := deps.Runner
	if runner == nil {
		runner = executil.Runner{Timeout: opts.Timeout}
	}
	scope := Scope{Start: end.Add(-opts.Since), End: end, Context: opts.Context, Namespace: opts.Namespace, Workload: opts.Workload, Repo: opts.Repo, AlertmanagerURL: opts.AlertmanagerURL}
	results := make([]SourceResult, len(sources))
	var workers sync.WaitGroup
	start := func(index int, collect func() SourceResult) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result := collect()
			// Accept each source before the shared deadline, preserving earlier successes
			// even when a different source later exhausts the budget.
			if len(result.Entries) > 0 || len(result.Summary.Context) > 0 || (len(result.Errors) == 0 && result.Summary.Status != "missing") {
				if err := collectionError(ctx); err != nil {
					result = externalFailure(sources[index], "timeout", "source collection deadline or cancellation reached")
				} else if clean, err := cleanSource(ctx, result); err != nil {
					result = externalFailure(sources[index], "timeout", "source output could not be safely retained before the deadline")
				} else {
					result = clean
				}
				if err := collectionError(ctx); err != nil {
					result = externalFailure(sources[index], "timeout", "source collection deadline or cancellation reached")
				}
			}
			results[index] = result
		}()
	}
	independent := scope
	start(5, func() SourceResult { return CollectGit(ctx, independent, runner) })
	if opts.Workload == "" {
		start(6, func() SourceResult { return CollectAlerts(ctx, independent, deps.HTTP) })
	}
	kube, kubeErr := deps.Kube, deps.KubeError
	if kube == nil && kubeErr == nil {
		kube, kubeErr = executil.NewKubernetes(ctx, executil.Runner{Timeout: opts.Timeout}, opts.Context)
	}
	selected := opts.Context
	if kube != nil && kubeErr == nil {
		selected = kube.Context()
		kube = pinnedKube{KubeRunner: kube, selected: selected}
	}
	safeContext, contextErr := safety.NewRedactor().RedactContext(ctx, selected)
	if contextErr != nil {
		safeContext = ""
	}
	var gaps []Gap
	var scopeErr error
	if kube != nil && kubeErr == nil {
		resolved, found, err := ResolveScope(ctx, opts, kube)
		resolved.Start, resolved.End = scope.Start, scope.End
		scope, gaps, scopeErr = resolved, found, err
	} else if opts.Workload != "" {
		scopeErr = kubeErr
	}
	if scopeErr != nil {
		scope = failedScope(scope)
		gaps = append(gaps, association("requested workload identity could not be resolved"))
	}
	collectors := []func(context.Context, Scope, KubeRunner) SourceResult{CollectEvents, CollectReplicaSets, CollectFluxKustomizations, CollectFluxHelmReleases, CollectArgo}
	for i, collector := range collectors {
		current := scope
		start(i, func() SourceResult {
			if kubeErr != nil {
				return kubeFailure(sources[i], kubeErr)
			}
			if current.Workload != "" && current.Target == nil {
				result := newKubeResult(sources[i])
				sourceGap(&result, "association", "requested workload is unresolved; source association unavailable")
				return finishKubeResult(result)
			}
			return collector(ctx, current, kube)
		})
	}
	if opts.Workload != "" {
		current := scope
		start(6, func() SourceResult {
			if current.Target == nil && current.AlertmanagerURL != "" {
				result := newKubeResult("alertmanager")
				sourceGap(&result, "association", "requested workload is unresolved; alert association unavailable")
				return finishKubeResult(result)
			}
			return CollectAlerts(ctx, current, deps.HTTP)
		})
	}
	workers.Wait()
	if scope.Workload != "" && scope.Target == nil {
		for i := range results {
			if sources[i] != "git" {
				sourceGap(&results[i], "association", "requested workload identity is unresolved; namespace scope was not substituted")
			}
		}
	}
	scope.Context = safeContext
	report := Merge(scope, results)
	report.Gaps = append(report.Gaps, gaps...)
	if scopeErr != nil {
		message, err := safety.NewRedactor().RedactContext(ctx, scopeErr.Error())
		if err != nil {
			message = "workload identity resolution failed before the collection deadline"
		}
		report.Errors = append(report.Errors, message)
	}
	return report, nil
}

func collectionError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The timer's cancellation callback may not have run yet.
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
