//go:build linux && (amd64 || arm64)

package coderuntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var errSandboxUnavailable = errors.New("Bun sandbox prerequisites unavailable")

func discoverSandbox(ctx context.Context) (sandboxConfig, error) {
	var cfg sandboxConfig
	if err := boundedSandboxContext(ctx); err != nil {
		return cfg, err
	}
	for name, dst := range map[string]*string{"bun": &cfg.bun, "bwrap": &cfg.bwrap, "systemd-run": &cfg.systemdRun, "systemctl": &cfg.systemctl, "prlimit": &cfg.prlimit, "env": &cfg.env} {
		path, err := exec.LookPath(name)
		if err != nil {
			return sandboxConfig{}, fmt.Errorf("%w: %s", errSandboxUnavailable, name)
		}
		*dst, err = validatedFile(path, true)
		if err != nil {
			return sandboxConfig{}, fmt.Errorf("%w: %s", errSandboxUnavailable, name)
		}
	}
	runtimeDir := fmt.Sprintf("/run/user/%d", os.Getuid())
	cfg.managerEnv = []string{"XDG_RUNTIME_DIR=" + runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus"}
	version, err := sandboxOutput(ctx, cfg.bun, []string{}, "--version")
	if err != nil || !supportedBunVersion(strings.TrimSpace(string(version))) {
		return sandboxConfig{}, fmt.Errorf("%w: Bun >=1.4.2 required", errSandboxUnavailable)
	}
	binary, err := elf.Open(cfg.bun)
	if err != nil {
		return sandboxConfig{}, fmt.Errorf("%w: Bun ELF", errSandboxUnavailable)
	}
	defer binary.Close()
	expected := elf.EM_X86_64
	if nativeSeccompArch == "arm64" {
		expected = elf.EM_AARCH64
	}
	if binary.Machine != expected || binary.Class != elf.ELFCLASS64 {
		return sandboxConfig{}, fmt.Errorf("%w: Bun architecture", errSandboxUnavailable)
	}
	loader := ""
	for _, p := range binary.Progs {
		if p.Type == elf.PT_INTERP {
			b, err := io.ReadAll(io.LimitReader(p.Open(), 4097))
			if err != nil || len(b) > 4096 {
				return sandboxConfig{}, fmt.Errorf("%w: Bun loader", errSandboxUnavailable)
			}
			loader = strings.TrimSuffix(string(b), "\x00")
		}
	}
	if !filepath.IsAbs(loader) {
		return sandboxConfig{}, fmt.Errorf("%w: dynamic Bun loader required", errSandboxUnavailable)
	}
	ldd, err := exec.LookPath("ldd")
	if err != nil {
		return sandboxConfig{}, fmt.Errorf("%w: ldd", errSandboxUnavailable)
	}
	ldd, err = validatedFile(ldd, true)
	if err != nil {
		return sandboxConfig{}, err
	}
	output, err := sandboxOutput(ctx, ldd, []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}, cfg.bun)
	if err != nil {
		return sandboxConfig{}, fmt.Errorf("%w: Bun libraries", errSandboxUnavailable)
	}
	libraries, err := parseLibraries(string(output))
	if err != nil {
		return sandboxConfig{}, err
	}
	libraries = append(libraries, loader)
	slices.Sort(libraries)
	libraries = slices.Compact(libraries)
	for _, target := range libraries {
		source, err := validatedFile(target, false)
		if err != nil {
			return sandboxConfig{}, fmt.Errorf("%w: library", errSandboxUnavailable)
		}
		cfg.libraries = append(cfg.libraries, sandboxLibrary{source, target})
	}
	if _, err = sandboxOutput(ctx, cfg.systemctl, cfg.managerEnv, "--user", "show", "--property=Version", "--value"); err != nil {
		return sandboxConfig{}, fmt.Errorf("%w: user manager", errSandboxUnavailable)
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return sandboxConfig{}, fmt.Errorf("%w: cgroup v2", errSandboxUnavailable)
	}
	for _, name := range []string{"memory", "pids", "cpu"} {
		if !slices.Contains(strings.Fields(string(controllers)), name) {
			return sandboxConfig{}, fmt.Errorf("%w: cgroup controller %s", errSandboxUnavailable, name)
		}
	}
	if err := ctx.Err(); err != nil {
		return sandboxConfig{}, err
	}
	return cfg, nil
}
func validatedFile(path string, executable bool) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("sandbox path must be absolute")
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || executable && info.Mode().Perm()&0111 == 0 {
		return "", errors.New("invalid sandbox file")
	}
	return path, nil
}

var bunVersionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

func supportedBunVersion(version string) bool {
	fields := bunVersionPattern.FindStringSubmatch(version)
	if fields == nil {
		return false
	}
	v := make([]uint64, 3)
	for i := range v {
		var err error
		v[i], err = strconv.ParseUint(fields[i+1], 10, 32)
		if err != nil {
			return false
		}
	}
	return v[0] > 1 || v[0] == 1 && (v[1] > 4 || v[1] == 4 && v[2] >= 2)
}
func parseLibraries(output string) ([]string, error) {
	var result []string
	for _, line := range strings.Split(output, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if strings.HasPrefix(f[0], "linux-vdso.so.") && len(f) == 2 {
			continue
		}
		path := ""
		if len(f) == 4 && f[1] == "=>" && strings.HasPrefix(f[3], "(0x") {
			path = f[2]
		} else if len(f) == 2 && strings.HasPrefix(f[1], "(0x") {
			path = f[0]
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, fmt.Errorf("%w: unresolved library", errSandboxUnavailable)
		}
		result = append(result, path)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("%w: no dynamic libraries", errSandboxUnavailable)
	}
	return result, nil
}

type sandboxBuffer struct {
	mu       sync.Mutex
	data     bytes.Buffer
	overflow bool
}

func (b *sandboxBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.data.Len()+len(p) > 32768 {
		b.overflow = true
		return 0, errors.New("sandbox command output limit")
	}
	return b.data.Write(p)
}
func sandboxOutput(ctx context.Context, path string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.WaitDelay = 100 * time.Millisecond
	var stdout, stderr sandboxBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stdout.overflow || stderr.overflow {
		return nil, errors.New("sandbox command output limit")
	}
	if err != nil {
		return nil, err
	}
	return stdout.data.Bytes(), nil
}
func sandboxArgs(cfg sandboxConfig, unit, bootstrap, program, socketPath string, remaining time.Duration) []string {
	seconds := min(61, max(1, int(math.Ceil(remaining.Seconds()))))
	args := []string{"--user", "--quiet", "--scope", "--unit", unit}
	for _, p := range []string{"MemoryMax=512M", "MemorySwapMax=0", "TasksMax=64", "CPUQuota=100%", fmt.Sprintf("RuntimeMaxSec=%ds", seconds)} {
		args = append(args, "--property", p)
	}
	args = append(args, cfg.prlimit, "--cpu=5:5", "--core=0:0", "--fsize=2097152:2097152", cfg.env, "-i", cfg.bwrap,
		"--unshare-all", "--unshare-user", "--unshare-cgroup", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv",
		"--setenv", "PATH", "/runtime", "--setenv", "TMPDIR", "/tmp", "--setenv", "BUN_RUNTIME_TRANSPILER_CACHE_PATH", "/tmp/cache",
		"--ro-bind", cfg.bun, "/runtime/bun", "--ro-bind", bootstrap, "/runtime/bootstrap.js", "--ro-bind", program, "/runtime/program.js")
	for _, library := range cfg.libraries {
		args = append(args, "--ro-bind", library.source, library.target)
	}
	if socketPath != "" {
		args = append(args, "--ro-bind", socketPath, "/run/egress.sock")
	}
	args = append(args, "--size", "16777216", "--tmpfs", "/tmp", "--dev", "/dev", "--proc", "/proc", "--dir", "/work", "--chdir", "/work", "--seccomp", "3", "--remount-ro", "/", "--", "/runtime/bun", "--no-env-file", "--no-install", "--no-macros", "/runtime/bootstrap.js")
	return args
}
func launchSandbox(ctx context.Context, cfg sandboxConfig, bootstrap, program, socketPath string) (*sandboxProcess, error) {
	if err := boundedSandboxContext(ctx); err != nil {
		return nil, err
	}
	for _, path := range []string{cfg.bun, cfg.bwrap, cfg.systemdRun, cfg.systemctl, cfg.prlimit, cfg.env} {
		if !filepath.IsAbs(path) {
			return nil, errors.New("invalid sandbox configuration")
		}
	}
	var err error
	bootstrap, err = validatedFile(bootstrap, false)
	if err != nil {
		return nil, err
	}
	program, err = validatedFile(program, false)
	if err != nil {
		return nil, err
	}
	if socketPath != "" {
		info, err := os.Lstat(socketPath)
		if err != nil || !filepath.IsAbs(socketPath) || info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("invalid sandbox socket")
		}
	}
	token := make([]byte, 16)
	if _, err = rand.Read(token); err != nil {
		return nil, err
	}
	unit := fmt.Sprintf("sre-bun-%x.scope", token)
	fd, err := unix.MemfdCreate("sre-bun-seccomp", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	filter := os.NewFile(uintptr(fd), "seccomp")
	transferred := false
	defer func() {
		if !transferred {
			filter.Close()
		}
	}()
	if _, err = filter.Write(marshalSeccomp(seccompPolicy(nativeSeccompArch))); err != nil {
		return nil, err
	}
	if _, err = filter.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	cmd := exec.Command(cfg.systemdRun, sandboxArgs(cfg, unit, bootstrap, program, socketPath, time.Until(deadline))...)
	cmd.Env = cfg.managerEnv
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.ExtraFiles = []*os.File{filter}
	// Own both ends explicitly: exec.Cmd.Wait must not discard buffered output.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if !transferred {
			inR.Close()
		}
	}()
	outR, outW, err := os.Pipe()
	if err != nil {
		inW.Close()
		return nil, err
	}
	defer func() {
		if !transferred {
			outW.Close()
		}
	}()
	errR, errW, err := os.Pipe()
	if err != nil {
		inW.Close()
		outR.Close()
		return nil, err
	}
	defer func() {
		if !transferred {
			errW.Close()
		}
	}()
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = errW
	done := make(chan struct{})
	p := &sandboxProcess{Stdin: inW, Stdout: outR, Stderr: errR, Done: done, cmd: cmd, cfg: cfg, unit: unit, closeGate: make(chan struct{}, 1)}
	started := make(chan error, 1)
	transferred = true
	go func() {
		// Linux parent-death signals follow the creating OS thread. Keep that
		// thread alive through the single Wait, even after the launch caller exits.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := ctx.Err()
		if err == nil {
			err = cmd.Start()
		}
		filter.Close()
		inR.Close()
		outW.Close()
		errW.Close()
		if err == nil {
			start, _ := processStartTime(cmd.Process.Pid)
			p.mu.Lock()
			p.process = cmd.Process
			p.startTime = start
			aborted := p.aborted
			p.mu.Unlock()
			if aborted {
				_ = cmd.Process.Kill()
			}
		} else {
			p.Abort()
		}
		started <- err
		if err == nil {
			err = cmd.Wait()
		}
		p.mu.Lock()
		p.waitErr = err
		p.mu.Unlock()
		close(done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			p.Abort()
		case <-done:
		}
	}()
	// Once ownership transfers, return the handle even on failure so the
	// caller can include partial startup in its shared bounded cleanup.
	select {
	case err := <-started:
		if ctxErr := ctx.Err(); ctxErr != nil {
			p.Abort()
			return p, ctxErr
		}
		return p, err
	case <-ctx.Done():
		p.Abort()
		return p, ctx.Err()
	}
}

func processStartTime(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return "", errors.New("invalid process stat")
	}
	f := strings.Fields(string(data[end+1:]))
	if len(f) < 20 {
		return "", errors.New("short process stat")
	}
	return f[19], nil
}

var scopeNamePattern = regexp.MustCompile(`^sre-bun-[0-9a-f]{32}\.scope$`)

func validateCgroupPath(group, unit string) (string, error) {
	if !scopeNamePattern.MatchString(unit) || !filepath.IsAbs(group) || filepath.Clean(group) != group || filepath.Base(group) != unit || strings.Contains(group, "\x00") {
		return "", errors.New("invalid sandbox cgroup ownership")
	}
	path := "/sys/fs/cgroup" + group
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if resolved != path || !strings.HasPrefix(resolved, "/sys/fs/cgroup/") {
		return "", errors.New("sandbox cgroup escapes root")
	}
	return path, nil
}
func (p *sandboxProcess) Verify(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			p.Abort()
		}
	}()
	if err = boundedSandboxContext(ctx); err != nil {
		return err
	}
	select {
	case <-p.Done:
		return errors.New("sandbox exited before verification")
	default:
	}
	group, err := sandboxOutput(ctx, p.cfg.systemctl, p.cfg.managerEnv, "--user", "show", p.unit, "--property=ControlGroup", "--value")
	if err != nil {
		return errors.New("sandbox cgroup lookup failed")
	}
	groupName := strings.TrimSpace(string(group))
	path, err := validateCgroupPath(groupName, p.unit)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.cgroup = path
	p.mu.Unlock()
	if err := verifyCgroupLimits(path); err != nil {
		return err
	}
	pid := p.cmd.Process.Pid
	start, err := processStartTime(pid)
	if err != nil || start == "" || start != p.startTime {
		return errors.New("sandbox process identity changed")
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil || exe != p.cfg.bwrap {
		return errors.New("sandbox launcher not ready")
	}
	membership, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil || strings.TrimSpace(string(membership)) != "0::"+groupName {
		return errors.New("sandbox process outside owned cgroup")
	}
	for resource, want := range map[int]uint64{unix.RLIMIT_CPU: 5, unix.RLIMIT_CORE: 0, unix.RLIMIT_FSIZE: 2097152} {
		var limit unix.Rlimit
		if err := unix.Prlimit(pid, resource, nil, &limit); err != nil || limit.Cur != want || limit.Max != want {
			return errors.New("sandbox degraded process rlimit")
		}
	}
	return ctx.Err()
}
func (p *sandboxProcess) Close(ctx context.Context) error {
	p.Abort()
	if err := boundedSandboxContext(ctx); err != nil {
		return err
	}
	select {
	case p.closeGate <- struct{}{}:
		defer func() { <-p.closeGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.closed {
		return nil
	}
	select {
	case <-p.Done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if !scopeNamePattern.MatchString(p.unit) {
		return errors.New("invalid sandbox scope ownership")
	}
	// Reaping the launcher precedes stopping its scope, so a startup in flight
	// cannot continue launching after the stop request.
	_, stopErr := sandboxOutput(ctx, p.cfg.systemctl, p.cfg.managerEnv, "--user", "stop", p.unit)
	output, err := sandboxOutput(ctx, p.cfg.systemctl, p.cfg.managerEnv, "--user", "show", p.unit, "--property=LoadState", "--property=ControlGroup", "--property=ActiveState")
	if err != nil {
		return fmt.Errorf("sandbox cleanup inspection: %w", err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			values[k] = v
		}
	}
	if values["LoadState"] != "not-found" {
		if stopErr != nil {
			return fmt.Errorf("sandbox scope stop: %w", stopErr)
		}
		if values["ActiveState"] != "inactive" && values["ActiveState"] != "failed" {
			return errors.New("sandbox scope remains active")
		}
		if group := values["ControlGroup"]; group != "" {
			path, err := validateCgroupPath(group, p.unit)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if err == nil {
				if err := emptySandboxCgroup(path); err != nil {
					return err
				}
			}
		}
	}
	p.mu.Lock()
	path := p.cgroup
	p.mu.Unlock()
	if path != "" {
		if err := emptySandboxCgroup(path); err != nil {
			return err
		}
	}
	if values["ActiveState"] == "failed" {
		if _, err := sandboxOutput(ctx, p.cfg.systemctl, p.cfg.managerEnv, "--user", "reset-failed", p.unit); err != nil {
			return err
		}
	}
	p.closed = true
	return nil
}
func emptySandboxCgroup(path string) error {
	data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "populated 0" {
			return nil
		}
	}
	return errors.New("sandbox cgroup still populated")
}

func verifyCgroupLimits(path string) error {
	for name, want := range map[string]string{"memory.max": "536870912", "memory.swap.max": "0", "pids.max": "64"} {
		data, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || strings.TrimSpace(string(data)) != want {
			return fmt.Errorf("sandbox degraded %s", name)
		}
	}
	cpu, err := os.ReadFile(filepath.Join(path, "cpu.max"))
	if err != nil {
		return err
	}
	fields := strings.Fields(string(cpu))
	if len(fields) != 2 {
		return errors.New("sandbox invalid CPU quota")
	}
	quota, e1 := strconv.ParseUint(fields[0], 10, 64)
	period, e2 := strconv.ParseUint(fields[1], 10, 64)
	if e1 != nil || e2 != nil || quota == 0 || period == 0 || quota > period {
		return errors.New("sandbox degraded CPU quota")
	}
	return nil
}
