package executil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type Kubernetes struct {
	runner  Runner
	context string
}

// Context returns the selected target so a collaborating service can share it.
func (k *Kubernetes) Context() string { return k.context }

func NewKubernetes(ctx context.Context, runner Runner, explicit string) (*Kubernetes, error) {
	selected := strings.TrimSpace(explicit)
	if selected == "" {
		selected = strings.TrimSpace(os.Getenv("SRE_KUBE_CONTEXT"))
	}
	if selected == "" {
		result := runner.Run(ctx, "kubectl", "config", "current-context")
		if result.Err != nil {
			return nil, fmt.Errorf("resolve Kubernetes context: %w", result.Err)
		}
		selected = strings.TrimSpace(result.Stdout)
	}
	if selected == "" || strings.ContainsAny(selected, "\r\n") {
		return nil, errors.New("Kubernetes context is missing or invalid")
	}
	return &Kubernetes{runner: runner, context: selected}, nil
}

func (k *Kubernetes) Run(ctx context.Context, args ...string) Result {
	if err := validatePinnedArgs(args); err != nil {
		return Result{Err: err}
	}
	return k.runner.Run(ctx, "kubectl", append([]string{"--context", k.context}, args...)...)
}

// Stream has Runner.Stream's raw-stdout contract with a pinned cluster target.
func (k *Kubernetes) Stream(ctx context.Context, output io.Writer, args ...string) Result {
	if err := validatePinnedArgs(args); err != nil {
		return Result{Err: err}
	}
	return k.runner.Stream(ctx, output, "kubectl", append([]string{"--context", k.context}, args...)...)
}

func validatePinnedArgs(args []string) error {
	for _, arg := range args {
		for _, flag := range []string{"--context", "--server", "--kubeconfig", "--cluster", "--user"} {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return errors.New("Kubernetes context and target are already pinned")
			}
		}
		if strings.HasPrefix(arg, "-s") && !strings.HasPrefix(arg, "--") {
			return errors.New("Kubernetes server is already pinned")
		}
	}
	return nil
}
