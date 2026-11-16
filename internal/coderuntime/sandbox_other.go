//go:build !linux || (!amd64 && !arm64)

package coderuntime

import (
	"context"
	"errors"
)

var errSandboxUnavailable = errors.New("Bun sandbox requires Linux amd64 or arm64")

func discoverSandbox(context.Context) (sandboxConfig, error) {
	return sandboxConfig{}, errSandboxUnavailable
}
func launchSandbox(context.Context, sandboxConfig, string, string, string) (*sandboxProcess, error) {
	return nil, errSandboxUnavailable
}
func (p *sandboxProcess) Verify(context.Context) error { return errSandboxUnavailable }
func (p *sandboxProcess) Close(context.Context) error  { return errSandboxUnavailable }
