//go:build linux && (amd64 || arm64)

package coderuntime

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// The older syscall numbers differ between the two supported Linux ABIs.
// Keeping both tables available lets the policy evaluator test both branches.
func seccompPolicy(arch string) []unix.SockFilter {
	audit, clone := uint32(0xc000003e), uint32(56)
	denied := []uint32{57, 58, 101, 155, 165, 166, 272, 298, 303, 304, 308, 310, 311, 317, 321, 322, 323}
	if arch == "arm64" {
		audit = 0xc00000b7
		clone = 220
		denied = []uint32{117, 41, 40, 39, 97, 241, 264, 265, 268, 270, 271, 277, 280, 281, 282}
	}
	if arch == nativeSeccompArch {
		audit = nativeAuditArch
		clone = unix.SYS_CLONE
		denied = nativeDeniedSyscalls()
	}
	denied = append(denied, unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER, unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT, unix.SYS_FSOPEN, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_FSPICK, unix.SYS_MOUNT_SETATTR, unix.SYS_PROCESS_MADVISE, unix.SYS_PROCESS_MRELEASE)
	ld := func(offset uint32) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: offset}
	}
	ret := func(value uint32) unix.SockFilter { return unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: value} }
	eq := func(value uint32, yes, no uint8) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: value, Jt: yes, Jf: no}
	}
	p := []unix.SockFilter{ld(4), eq(audit, 1, 0), ret(unix.SECCOMP_RET_KILL_PROCESS), ld(0)}
	if arch == "amd64" {
		p = append(p, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jf: 1}, ret(unix.SECCOMP_RET_KILL_PROCESS))
	}
	for _, nr := range denied {
		p = append(p, eq(nr, 0, 1), ret(unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)))
	}
	p = append(p, eq(unix.SYS_CLONE3, 0, 1), ret(unix.SECCOMP_RET_ERRNO|uint32(unix.ENOSYS)), eq(clone, 0, 5), ld(16),
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: unix.CLONE_THREAD, Jt: 1}, ret(unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)),
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: unix.CLONE_NEWCGROUP | unix.CLONE_NEWIPC | unix.CLONE_NEWNET | unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWTIME | unix.CLONE_NEWUSER | unix.CLONE_NEWUTS, Jf: 1}, ret(unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)), ret(unix.SECCOMP_RET_ALLOW))
	return p
}
func commonDeniedSyscalls() []uint32 {
	return []uint32{unix.SYS_PTRACE, unix.SYS_PIVOT_ROOT, unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_UNSHARE, unix.SYS_PERF_EVENT_OPEN, unix.SYS_NAME_TO_HANDLE_AT, unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_SETNS, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_SECCOMP, unix.SYS_BPF, unix.SYS_EXECVEAT, unix.SYS_USERFAULTFD}
}
func marshalSeccomp(p []unix.SockFilter) []byte {
	data := make([]byte, 8*len(p))
	for n, i := range p {
		b := data[n*8:]
		binary.NativeEndian.PutUint16(b, i.Code)
		b[2] = i.Jt
		b[3] = i.Jf
		binary.NativeEndian.PutUint32(b[4:], i.K)
	}
	return data
}
