package logs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
)

type PodOptions struct {
	Context   string
	Namespace string
	Pod       string
	Container string
	Previous  bool
	Since     time.Duration
	Options   Options
}

var resourceName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

func ValidatePod(opts PodOptions) error {
	if !resourceName.MatchString(opts.Pod) || !resourceName.MatchString(opts.Namespace) {
		return errors.New("pod and namespace names are required and must be valid Kubernetes names")
	}
	if opts.Container != "" && !resourceName.MatchString(opts.Container) {
		return errors.New("invalid container name")
	}
	if opts.Since < 0 {
		return errors.New("since must be a positive duration")
	}
	if opts.Options.MaxPatterns < 0 {
		return errors.New("max-patterns must be positive")
	}
	return nil
}

// FoldPod collects a finite snapshot using the pinned Kubernetes executor.
// A failed producer never returns a misleading successful partial report.
func FoldPod(ctx context.Context, opts PodOptions) (Report, error) {
	if err := ValidatePod(opts); err != nil {
		return Report{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, executil.DefaultTimeout)
	defer cancel()
	kube, err := executil.NewKubernetes(ctx, executil.Runner{}, opts.Context)
	if err != nil {
		return Report{}, err
	}
	args := []string{"logs", "--namespace=" + opts.Namespace, "--timestamps=true"}
	if opts.Container != "" {
		args = append(args, "--container="+opts.Container)
	}
	if opts.Previous {
		args = append(args, "--previous=true")
	}
	if opts.Since > 0 {
		args = append(args, "--since="+opts.Since.String())
	}
	args = append(args, "pod/"+opts.Pod)
	reader, writer := io.Pipe()
	done := make(chan executil.Result, 1)
	go func() {
		result := kube.Stream(ctx, writer, args...)
		_ = writer.CloseWithError(result.Err)
		done <- result
	}()
	report, foldErr := Fold(reader, opts.Options)
	canceledForFold := foldErr != nil && ctx.Err() == nil
	_ = reader.Close()
	if foldErr != nil {
		cancel()
	}
	result := <-done
	// Cleanup cancellation must not hide the input error that triggered it.
	if canceledForFold && errors.Is(result.Err, context.Canceled) {
		return Report{}, foldErr
	}
	if result.Err != nil {
		return Report{}, fmt.Errorf("collect pod logs: %w", result.Err)
	}
	if foldErr != nil {
		return Report{}, foldErr
	}
	return report, nil
}
