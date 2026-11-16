package coderuntime

import (
	"context"
	"testing"
	"time"
)

func TestSandboxRejectsUnboundedLaunch(t *testing.T) {
	if p, err := launchSandbox(context.Background(), sandboxConfig{}, "", "", ""); err == nil || p != nil {
		t.Fatal("unbounded launch accepted")
	}
}
func TestSandboxMissingPrerequisites(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := discoverSandbox(ctx); err == nil {
		t.Fatal("missing prerequisites accepted")
	}
}
func TestSandboxCanceledDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := discoverSandbox(ctx); err == nil {
		t.Fatal("canceled discovery accepted")
	}
}
