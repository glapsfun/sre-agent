//go:build linux && arm64

package coderuntime

import "golang.org/x/sys/unix"

const nativeSeccompArch = "arm64"
const nativeAuditArch = unix.AUDIT_ARCH_AARCH64

func nativeDeniedSyscalls() []uint32 { return commonDeniedSyscalls() }
