//go:build linux && amd64

package coderuntime

import "golang.org/x/sys/unix"

const nativeSeccompArch = "amd64"
const nativeAuditArch = unix.AUDIT_ARCH_X86_64

func nativeDeniedSyscalls() []uint32 {
	return append(commonDeniedSyscalls(), unix.SYS_FORK, unix.SYS_VFORK)
}
