package executil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

type Kubernetes struct {
	runner  Runner
	context string
}

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
	for _, arg := range args {
		for _, flag := range []string{"--context", "--server", "--kubeconfig", "--cluster", "--user"} {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return Result{Err: errors.New("Kubernetes context and target are already pinned")}
			}
		}
		if strings.HasPrefix(arg, "-s") && !strings.HasPrefix(arg, "--") {
			return Result{Err: errors.New("Kubernetes server is already pinned")}
		}
	}
	return k.runner.Run(ctx, "kubectl", append([]string{"--context", k.context}, args...)...)
}
