//go:build linux && (amd64 || arm64)

package coderuntime

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func sandboxFixture(t *testing.T, body string, duration time.Duration) (*sandboxProcess, *bufio.Reader) {
	t.Helper()
	if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") != "1" && os.Getenv("SRE_TEST_BUN_RUNTIME") != "1" {
		t.Skip("set SRE_TEST_BUN_RUNTIME=1 to opt in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	t.Cleanup(cancel)
	cfg, err := discoverSandbox(ctx)
	if err != nil {
		if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	dir := t.TempDir()
	bootstrap, program := filepath.Join(dir, "bootstrap.js"), filepath.Join(dir, "program.js")
	if err := os.WriteFile(bootstrap, []byte(`console.log("ready"); await Bun.stdin.stream().getReader().read(); await import("/runtime/program.js");`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := launchSandbox(ctx, cfg, bootstrap, program, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := p.Close(cleanup); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	r := bufio.NewReader(p.Stdout)
	line, err := r.ReadString('\n')
	if err != nil || line != "ready\n" {
		stderr, _ := io.ReadAll(p.Stderr)
		t.Fatalf("ready: %q %v stderr=%s", line, err, stderr)
	}
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"memory.max", "memory.swap.max", "pids.max", "cpu.max"} {
		data, err := os.ReadFile(filepath.Join(p.cgroup, name))
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("SRE_SANDBOX_PARENT_HELPER") != "1" {
			t.Logf("%s=%s", name, strings.TrimSpace(string(data)))
		}
	}
	return p, r
}
func proceed(t *testing.T, p *sandboxProcess) {
	t.Helper()
	if _, err := io.WriteString(p.Stdin, "proceed\n"); err != nil {
		t.Fatal(err)
	}
	if err := p.Stdin.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestSandboxIsolation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	p, r := sandboxFixture(t, `import fs from "node:fs"; import net from "node:net";
const o={env:fs.readFileSync("/proc/self/environ","utf8").split("\0").filter(Boolean).sort(),initEnv:fs.readFileSync("/proc/1/environ","utf8"),pid:process.pid,procs:fs.readdirSync("/proc").filter(x=>/^\d+$/.test(x)),limits:fs.readFileSync("/proc/self/limits","utf8")};
for(const [key,path] of [["host","/etc/passwd"],["initRoot","/proc/1/root/etc/passwd"]]) {try{o[key]=fs.readFileSync(path,"utf8")}catch(e){o[key]=e.code}}
try{fs.writeFileSync("/work/forbidden","x");o.root="writable"}catch(e){o.root=e.code}
try{Bun.spawnSync(["/runtime/bun","--version"]);o.spawn="allowed"}catch(e){o.spawn=e.code}
fs.writeFileSync("/tmp/allowed","scratch");o.scratch=fs.readFileSync("/tmp/allowed","utf8");
o.network=await new Promise(resolve=>{const s=net.createConnection({host:"127.0.0.1",port:`+strconv.Itoa(port)+`});s.on("connect",()=>{s.destroy();resolve("connected")});s.on("error",e=>resolve(e.code));s.setTimeout(300,()=>{s.destroy();resolve("timeout")})});
console.log(JSON.stringify(o));`, 10*time.Second)
	proceed(t, p)
	var o struct {
		Env                                                            []string
		InitEnv, Host, InitRoot, Root, Spawn, Scratch, Network, Limits string
		PID                                                            int
		Procs                                                          []string
	}
	if err := json.NewDecoder(r).Decode(&o); err != nil {
		t.Fatal(err)
	}
	want := []string{"BUN_RUNTIME_TRANSPILER_CACHE_PATH=/tmp/cache", "PATH=/runtime", "PWD=/work", "TMPDIR=/tmp"}
	if !reflect.DeepEqual(o.Env, want) || o.InitEnv != "" {
		t.Fatalf("environments: %+v", o)
	}
	if o.Host != "ENOENT" || o.InitRoot != "ENOENT" || o.Root != "EROFS" || o.Spawn != "EPERM" || o.Scratch != "scratch" || o.Network != "ECONNREFUSED" {
		t.Fatalf("isolation: %+v", o)
	}
	for _, limit := range []string{"Max cpu time 5 5 seconds", "Max file size 2097152 2097152 bytes", "Max core file size 0 0 bytes"} {
		if !strings.Contains(strings.Join(strings.Fields(o.Limits), " "), limit) {
			t.Errorf("missing inherited limit %q in %s", limit, o.Limits)
		}
	}
	if o.PID > 4 || len(o.Procs) > 3 {
		t.Fatalf("host proc exposed: %+v", o)
	}
	if p.WaitErr() != nil {
		t.Fatal(p.WaitErr())
	}
	t.Logf("observations: %+v", o)
}
func TestSandboxMemory(t *testing.T) {
	for _, kind := range []string{"native", "worker", "wasm"} {
		t.Run(kind, func(t *testing.T) {
			body := `const a=[];for(;;){const b=new Uint8Array(16*1024*1024);b.fill(7);a.push(b)}`
			if kind == "worker" {
				body = `const w=new Worker(URL.createObjectURL(new Blob([` + fmt.Sprintf("%q", body) + `],{type:"text/javascript"})));await new Promise(()=>{});`
			}
			if kind == "wasm" {
				body = `const m=new WebAssembly.Memory({initial:1,maximum:16384});for(;;){m.grow(256);new Uint8Array(m.buffer).fill(7)}`
			}
			p, _ := sandboxFixture(t, body, 15*time.Second)
			proceed(t, p)
			if p.WaitErr() == nil {
				t.Fatal("allocation survived")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := sandboxOutput(ctx, p.cfg.systemctl, p.cfg.managerEnv, "--user", "show", p.unit, "--property=Result", "--value")
			if err != nil || strings.TrimSpace(string(result)) != "oom-kill" {
				t.Fatalf("OOM result: %s %v", result, err)
			}
			t.Logf("%s: exit=%v scope result=%s", kind, p.WaitErr(), result)
		})
	}
}
func TestSandboxCPU(t *testing.T) {
	p, _ := sandboxFixture(t, `for(;;){}`, 12*time.Second)
	proceed(t, p)
	var used uint64
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
loop:
	for {
		select {
		case <-p.Done:
			break loop
		case <-timer.C:
			data, err := os.ReadFile(filepath.Join(p.cgroup, "cpu.stat"))
			if err == nil {
				for _, line := range strings.Split(string(data), "\n") {
					f := strings.Fields(line)
					if len(f) == 2 && f[0] == "usage_usec" {
						n, _ := strconv.ParseUint(f[1], 10, 64)
						used = max(used, n)
					}
				}
			}
		}
	}
	if p.WaitErr() == nil {
		t.Fatal("CPU loop survived")
	}
	if used < 4000000 || used > 5200000 {
		t.Fatalf("CPU usage_usec=%d exit=%v", used, p.WaitErr())
	}
	t.Logf("CPU usage_usec=%d exit=%v", used, p.WaitErr())
}
func TestSandboxCleanup(t *testing.T) {
	for _, mode := range []string{"deadline", "readiness", "output", "abort"} {
		t.Run(mode, func(t *testing.T) {
			duration := 10 * time.Second
			if mode == "deadline" {
				duration = 700 * time.Millisecond
			}
			body := `setInterval(()=>{},1000);`
			if mode == "output" {
				body = `for(;;)process.stdout.write("x".repeat(65536));`
			}
			p, _ := sandboxFixture(t, body, duration)
			if mode != "readiness" {
				proceed(t, p)
			}
			if mode == "output" {
				if _, err := io.CopyN(io.Discard, p.Stdout, 131072); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "deadline" {
				<-p.Done
			} else {
				p.Abort()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := p.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if err := p.Close(ctx); err != nil {
				t.Fatal("repeat close", err)
			}
			assertScopeEmpty(t, p.cgroup)
			t.Logf("%s: empty %s", mode, p.cgroup)
		})
	}
}
func assertScopeEmpty(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil || !strings.Contains(string(data), "populated 0") {
		t.Fatalf("scope not empty %q: %s %v", path, data, err)
	}
}
func TestSandboxParentDeath(t *testing.T) {
	if os.Getenv("SRE_SANDBOX_PARENT_HELPER") == "1" {
		p, _ := sandboxFixture(t, `setInterval(()=>{},1000)`, 15*time.Second)
		fmt.Printf("OWNED %s %s\n", p.unit, p.cgroup)
		<-p.Done
		os.Exit(2)
	}
	if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") != "1" && os.Getenv("SRE_TEST_BUN_RUNTIME") != "1" {
		t.Skip("opt in")
	}
	discovery, cancelDiscovery := context.WithTimeout(context.Background(), time.Second)
	cfg, err := discoverSandbox(discovery)
	cancelDiscovery()
	if err != nil {
		if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSandboxParentDeath$")
	cmd.Env = append(os.Environ(), "SRE_SANDBOX_PARENT_HELPER=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	r := bufio.NewReader(out)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[0] != "OWNED" {
		t.Fatalf("helper: %s", line)
	}
	if !scopeNamePattern.MatchString(parts[1]) {
		t.Fatal("invalid helper scope")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		sandboxOutput(ctx, cfg.systemctl, cfg.managerEnv, "--user", "stop", parts[1])
		sandboxOutput(ctx, cfg.systemctl, cfg.managerEnv, "--user", "reset-failed", parts[1])
		assertScopeEmpty(t, parts[2])
	})
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, e := os.ReadFile(filepath.Join(parts[2], "cgroup.events"))
		if os.IsNotExist(e) || strings.Contains(string(data), "populated 0") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assertScopeEmpty(t, parts[2])
	t.Logf("killed owner: empty %s", parts[2])

}
func TestSeccompPolicy(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		policy := seccompPolicy(arch)
		nr := uint32(56)
		audit := uint32(0xc000003e)
		if arch == "arm64" {
			nr = 220
			audit = 0xc00000b7
		}
		cases := []struct {
			name                  string
			nr, arch, flags, want uint32
		}{
			{"wrong ABI", 0, 0, 0, 0x80000000}, {"ordinary read", 0, audit, 0, 0x7fff0000}, {"process clone", nr, audit, 0, 0x50001}, {"thread clone", nr, audit, 0x10000, 0x7fff0000}, {"namespace thread", nr, audit, 0x10000 | 0x40000000, 0x50001}, {"clone3", 435, audit, 0, 0x50026}, {"io_uring", 425, audit, 0, 0x50001}, {"process_madvise", 440, audit, 0, 0x50001}, {"process_mrelease", 448, audit, 0, 0x50001}}
		if arch == "amd64" {
			cases = append(cases, struct {
				name                  string
				nr, arch, flags, want uint32
			}{"x32", 0x40000000, audit, 0, 0x80000000})
		}
		for _, c := range cases {
			t.Run(arch+"/"+c.name, func(t *testing.T) {
				if got := evaluateFilter(t, policy, c.nr, c.arch, c.flags); got != c.want {
					t.Fatalf("got %#x want %#x", got, c.want)
				}
			})
		}
	}
	data := marshalSeccomp(seccompPolicy("amd64"))
	if len(data)%8 != 0 || binary.NativeEndian.Uint16(data) != unix.BPF_LD|unix.BPF_W|unix.BPF_ABS {
		t.Fatal("invalid filter serialization")
	}
}
func evaluateFilter(t *testing.T, p []unix.SockFilter, nr, arch, flags uint32) uint32 {
	t.Helper()
	var a uint32
	for pc := 0; pc < len(p); pc++ {
		i := p[pc]
		switch i.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			switch i.K {
			case 0:
				a = nr
			case 4:
				a = arch
			case 16:
				a = flags
			default:
				t.Fatalf("load offset %d", i.K)
			}
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if a == i.K {
				pc += int(i.Jt)
			} else {
				pc += int(i.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if a&i.K != 0 {
				pc += int(i.Jt)
			} else {
				pc += int(i.Jf)
			}
		case unix.BPF_RET | unix.BPF_K:
			return i.K
		default:
			t.Fatalf("opcode %#x", i.Code)
		}
	}
	t.Fatal("no return")
	return 0
}
func TestSandboxVersionAndLibraryParsing(t *testing.T) {
	for _, v := range []string{"1.4.2", "1.4.3", "2.0.0"} {
		if !supportedBunVersion(v) {
			t.Errorf("rejected %s", v)
		}
	}
	for _, v := range []string{"1.4.1", "garbage", "1.4.2-canary", "1.4", "1.4.2\nextra"} {
		if supportedBunVersion(v) {
			t.Errorf("accepted %s", v)
		}
	}
	for _, output := range []string{"libc.so => not found", "libc.so => relative (0x123)", "unexpected"} {
		if _, err := parseLibraries(output); err == nil {
			t.Errorf("accepted %q", output)
		}
	}
	libs, err := parseLibraries("linux-vdso.so.1 (0x123)\nlibc.so.6 => /lib/libc.so.6 (0x123)\n/lib/ld-linux.so (0x123)\n")
	if err != nil || !reflect.DeepEqual(libs, []string{"/lib/libc.so.6", "/lib/ld-linux.so"}) {
		t.Fatalf("libraries %v %v", libs, err)
	}
}
func TestSandboxCgroupPath(t *testing.T) {
	for _, path := range []string{"/", "/user.slice/../escape.scope", "relative.scope", "/user.slice/other.scope", "/sys/fs/cgroup"} {
		if _, err := validateCgroupPath(path, "sre-bun-0123456789abcdef0123456789abcdef.scope"); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
}
func TestSandboxScratchAndFileLimits(t *testing.T) {
	p, r := sandboxFixture(t, `import fs from "node:fs";
const o={};const fd=fs.openSync("/tmp/large","w");let n=0;try{for(;;)n+=fs.writeSync(fd,Buffer.alloc(65536))}catch(e){o.fileError=e.code}fs.closeSync(fd);o.fileSize=fs.statSync("/tmp/large").size;fs.unlinkSync("/tmp/large");
let total=0;try{for(let i=0;i<32;i++){const f=fs.openSync("/tmp/f"+i,"w");try{for(let j=0;j<16;j++)total+=fs.writeSync(f,Buffer.alloc(65536))}finally{fs.closeSync(f)}}}catch(e){o.scratchError=e.code}o.scratchBytes=total;o.cores=fs.readdirSync("/work").filter(x=>x.startsWith("core"));console.log(JSON.stringify(o));`, 10*time.Second)
	proceed(t, p)
	var o struct {
		FileError, ScratchError string
		FileSize, ScratchBytes  int64
		Cores                   []string
	}
	if err := json.NewDecoder(r).Decode(&o); err != nil {
		t.Fatal(err)
	}
	if o.FileError != "EFBIG" || o.FileSize != 2097152 || o.ScratchError != "ENOSPC" || o.ScratchBytes > 16777216 || len(o.Cores) != 0 {
		t.Fatalf("limits: %+v", o)
	}
	if p.WaitErr() != nil {
		t.Fatal(p.WaitErr())
	}
	t.Logf("limits: %+v", o)
}
func TestSandboxCommandPolicy(t *testing.T) {
	cfg := sandboxConfig{bun: "/trusted/bun", bwrap: "/usr/bin/bwrap", systemdRun: "/usr/bin/systemd-run", systemctl: "/usr/bin/systemctl", prlimit: "/usr/bin/prlimit", env: "/usr/bin/env", libraries: []sandboxLibrary{{"/lib/loader", "/lib/loader"}}}
	args := sandboxArgs(cfg, "sre-bun-0123456789abcdef0123456789abcdef.scope", "/host/bootstrap", "/host/program", "", 600*time.Millisecond)
	joined := strings.Join(args, " ")
	for _, fragment := range []string{"--scope --unit sre-bun-", "MemoryMax=512M", "MemorySwapMax=0", "TasksMax=64", "CPUQuota=100%", "RuntimeMaxSec=1s", "/usr/bin/prlimit --cpu=5:5 --core=0:0 --fsize=2097152:2097152 /usr/bin/env -i /usr/bin/bwrap", "--unshare-all --unshare-user --unshare-cgroup", "--die-with-parent --new-session --cap-drop ALL --clearenv", "--size 16777216 --tmpfs /tmp", "--seccomp 3", "--remount-ro / -- /runtime/bun --no-env-file --no-install --no-macros /runtime/bootstrap.js"} {
		if !strings.Contains(joined, fragment) {
			t.Errorf("missing boundary %q", fragment)
		}
	}
	if strings.Contains(joined, "egress.sock") {
		t.Fatal("offline sandbox has socket")
	}
	args = sandboxArgs(cfg, "unit", "/bootstrap", "/program", "/host/socket", time.Hour)
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "RuntimeMaxSec=61s") || !strings.Contains(joined, "--ro-bind /host/socket /run/egress.sock") {
		t.Fatal(joined)
	}
}
func TestSandboxFailedVerificationAborts(t *testing.T) {
	p, _ := sandboxFixture(t, `throw new Error("must not execute")`, 10*time.Second)
	p.startTime = "incorrect"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Verify(ctx); err == nil {
		t.Fatal("changed identity accepted")
	}
	select {
	case <-p.Done:
	case <-ctx.Done():
		t.Fatal("failed verification left process alive")
	}
	if _, err := io.WriteString(p.Stdin, "proceed\n"); err == nil {
		t.Fatal("input remained open")
	}
}
func TestSandboxOutputBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	path, err := exec.LookPath("head")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sandboxOutput(ctx, path, []string{}, "-c", "65536", "/dev/zero"); err == nil {
		t.Fatal("unbounded command output accepted")
	}
}
func TestSandboxEarlyAbort(t *testing.T) {
	if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") != "1" && os.Getenv("SRE_TEST_BUN_RUNTIME") != "1" {
		t.Skip("opt in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg, err := discoverSandbox(ctx)
	if err != nil {
		if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	dir := t.TempDir()
	bootstrap := filepath.Join(dir, "bootstrap.js")
	if err := os.WriteFile(bootstrap, []byte(`setInterval(()=>{},1000)`), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		p, err := launchSandbox(ctx, cfg, bootstrap, bootstrap, "")
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(i%6) * 3 * time.Millisecond)
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = p.Close(cleanup)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		check, cancel := context.WithTimeout(context.Background(), time.Second)
		output, err := sandboxOutput(check, cfg.systemctl, cfg.managerEnv, "--user", "show", p.unit, "--property=ActiveState", "--value")
		cancel()
		if err != nil || strings.TrimSpace(string(output)) != "inactive" {
			t.Fatalf("late scope: %s %v", output, err)
		}
	}
}
func TestSandboxConcurrentClose(t *testing.T) {
	p, _ := sandboxFixture(t, `setInterval(()=>{},1000)`, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { done <- p.Close(ctx) }()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	assertScopeEmpty(t, p.cgroup)
}
func TestSandboxCloseRequiresBoundedContext(t *testing.T) {
	p, _ := sandboxFixture(t, `setInterval(()=>{},1000)`, 10*time.Second)
	if err := p.Close(context.Background()); err == nil {
		t.Fatal("unbounded cleanup accepted")
	}
}
func TestSeccompDeniedSyscalls(t *testing.T) {
	tables := map[string][]uint32{
		"amd64": {57, 58, 101, 155, 165, 166, 272, 298, 303, 304, 308, 310, 311, 317, 321, 322, 323},
		"arm64": {117, 41, 40, 39, 97, 241, 264, 265, 268, 270, 271, 277, 280, 281, 282},
	}
	for arch, numbers := range tables {
		audit := uint32(0xc000003e)
		if arch == "arm64" {
			audit = 0xc00000b7
		}
		numbers = append(numbers, 425, 426, 427, 428, 429, 430, 431, 432, 433, 440, 442, 448)
		for _, nr := range numbers {
			if got := evaluateFilter(t, seccompPolicy(arch), nr, audit, 0); got != 0x50001 {
				t.Errorf("%s syscall %d allowed: %#x", arch, nr, got)
			}
		}
	}
}
func TestSandboxCoreLimit(t *testing.T) {
	p, _ := sandboxFixture(t, `process.chdir("/tmp");process.abort();`, 10*time.Second)
	data, err := os.ReadFile(filepath.Join(p.cgroup, "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	var scratch *os.File
	for _, pid := range strings.Fields(string(data)) {
		root := "/proc/" + pid + "/root/tmp"
		candidate, e := os.Open(root)
		if e != nil {
			continue
		}
		// Only the private mount has the 16MiB filesystem limit.
		var stat unix.Statfs_t
		if unix.Fstatfs(int(candidate.Fd()), &stat) == nil && uint64(stat.Blocks)*uint64(stat.Bsize) == 16777216 {
			scratch = candidate
			break
		}
		candidate.Close()
	}
	if scratch == nil {
		t.Fatal("cannot inspect private scratch")
	}
	defer scratch.Close()
	proceed(t, p)
	if p.WaitErr() == nil {
		t.Fatal("abort survived")
	}
	names, err := scratch.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "core") {
			t.Errorf("core file created: %s", name)
		}
	}
	t.Logf("abort exit=%v private scratch=%v", p.WaitErr(), names)
}
func TestSandboxRejectsDegradedCgroupLimits(t *testing.T) {
	for _, c := range []struct{ name, value string }{
		{"memory.max", "max"}, {"memory.max", "1073741824"}, {"memory.swap.max", "1"}, {"pids.max", "128"},
		{"cpu.max", "max 100000"}, {"cpu.max", "100001 100000"}, {"cpu.max", "0 100000"}, {"cpu.max", "1 0"}, {"cpu.max", "invalid"},
	} {
		t.Run(c.name+"/"+c.value, func(t *testing.T) {
			dir := t.TempDir()
			for name, value := range map[string]string{"memory.max": "536870912", "memory.swap.max": "0", "pids.max": "64", "cpu.max": "100000 100000"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := verifyCgroupLimits(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, c.name), []byte(c.value), 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifyCgroupLimits(dir); err == nil {
				t.Fatal("degraded boundary accepted")
			}
		})
	}
	if err := verifyCgroupLimits(t.TempDir()); err == nil {
		t.Fatal("missing cgroup controls accepted")
	}
}

func TestSandboxStartupParentDeath(t *testing.T) {
	if stage := os.Getenv("SRE_SANDBOX_STARTUP_STAGE"); stage != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cfg, err := discoverSandbox(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if stage == "before-scope" {
			cfg.systemdRun = os.Getenv("SRE_SANDBOX_STARTUP_WRAPPER")
		} else {
			cfg.prlimit = os.Getenv("SRE_SANDBOX_STARTUP_WRAPPER")
		}
		fixture := os.Getenv("SRE_SANDBOX_STARTUP_FIXTURE")
		p, err := launchSandbox(ctx, cfg, fixture, fixture, "")
		if err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(p.Stdout).ReadString('\n')
		if err != nil || line != "PAUSED\n" {
			t.Fatalf("startup pause: %q %v", line, err)
		}
		group, err := sandboxOutput(ctx, cfg.systemctl, cfg.managerEnv, "--user", "show", p.unit, "--property=ControlGroup", "--value")
		if err != nil {
			t.Fatal(err)
		}
		path := ""
		if strings.TrimSpace(string(group)) != "" {
			path, err = validateCgroupPath(strings.TrimSpace(string(group)), p.unit)
			if err != nil {
				t.Fatal(err)
			}
		}
		if stage == "before-bwrap" && path == "" {
			t.Fatal("pause did not enter scope")
		}
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", p.cmd.Process.Pid))
		if err != nil || exe == cfg.bwrap {
			t.Fatalf("not paused before bwrap: %s %v", exe, err)
		}
		json.NewEncoder(os.Stdout).Encode(struct {
			PID         int
			Unit, Group string
		}{p.cmd.Process.Pid, p.unit, path})
		<-p.Done
		os.Exit(2)
	}
	if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") != "1" && os.Getenv("SRE_TEST_BUN_RUNTIME") != "1" {
		t.Skip("opt in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg, err := discoverSandbox(ctx)
	if err != nil {
		if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	for _, stage := range []string{"before-scope", "before-bwrap"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			fifo := filepath.Join(dir, "pause")
			if err := unix.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			keepOpen, err := os.OpenFile(fifo, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer keepOpen.Close()
			target := cfg.systemdRun
			if stage == "before-bwrap" {
				target = cfg.prlimit
			}
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
			wrapper := filepath.Join(dir, "pause.sh")
			script := "#!/bin/sh\nprintf 'PAUSED\\n'\nread -r proceed < " + quote(fifo) + "\nexec " + quote(target) + " \"$@\"\n"
			if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			fixture := filepath.Join(dir, "fixture.js")
			if err := os.WriteFile(fixture, []byte(`setInterval(()=>{},1000)`), 0600); err != nil {
				t.Fatal(err)
			}
			owner := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSandboxStartupParentDeath$")
			owner.Env = append(os.Environ(), "SRE_SANDBOX_STARTUP_STAGE="+stage, "SRE_SANDBOX_STARTUP_WRAPPER="+wrapper, "SRE_SANDBOX_STARTUP_FIXTURE="+fixture)
			output, err := owner.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			owner.Stderr = os.Stderr
			if err := owner.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { owner.Process.Kill(); owner.Wait() }()
			var owned struct {
				PID         int
				Unit, Group string
			}
			if err := json.NewDecoder(output).Decode(&owned); err != nil {
				t.Fatal(err)
			}
			if !scopeNamePattern.MatchString(owned.Unit) || owned.PID <= 0 {
				t.Fatal("invalid owned process")
			}
			launcher, err := os.FindProcess(owned.PID)
			if err != nil {
				t.Fatal(err)
			}
			defer launcher.Release()
			defer func() {
				launcher.Kill()
				cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				sandboxOutput(cleanup, cfg.systemctl, cfg.managerEnv, "--user", "stop", owned.Unit)
				sandboxOutput(cleanup, cfg.systemctl, cfg.managerEnv, "--user", "reset-failed", owned.Unit)
			}()
			if err := owner.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			exited := false
			for time.Now().Before(deadline) {
				data, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", owned.PID))
				if os.IsNotExist(e) {
					exited = true
					break
				}
				if e == nil {
					end := strings.LastIndexByte(string(data), ')')
					fields := strings.Fields(string(data)[end+1:])
					if len(fields) > 0 && fields[0] == "Z" {
						exited = true
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !exited {
				t.Errorf("launcher %d survived owner death at %s", owned.PID, stage)
			}
			if owned.Group != "" {
				assertScopeEmpty(t, owned.Group)
			} else {
				group, e := sandboxOutput(ctx, cfg.systemctl, cfg.managerEnv, "--user", "show", owned.Unit, "--property=ControlGroup", "--value")
				if e != nil || strings.TrimSpace(string(group)) != "" {
					t.Errorf("late scope: %q %v", group, e)
				}
			}
			t.Logf("%s: launcher_exited=%v group=%q", stage, exited, owned.Group)
		})
	}
}

func TestSandboxLaunchCallerReturns(t *testing.T) {
	if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") != "1" && os.Getenv("SRE_TEST_BUN_RUNTIME") != "1" {
		t.Skip("opt in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, err := discoverSandbox(ctx)
	if err != nil {
		if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.js")
	if err := os.WriteFile(fixture, []byte(`console.log("ready");await Bun.stdin.stream().getReader().read();console.log("finished");`), 0600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		p   *sandboxProcess
		err error
	}
	started := make(chan result, 1)
	returned := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer close(returned)
		p, err := launchSandbox(ctx, cfg, fixture, fixture, "")
		started <- result{p, err}
	}()
	r := <-started
	<-returned
	if r.err != nil {
		t.Fatal(r.err)
	}
	p := r.p
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := p.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	reader := bufio.NewReader(p.Stdout)
	line, err := reader.ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("caller returned before ready: %q %v", line, err)
	}
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	proceed(t, p)
	line, err = reader.ReadString('\n')
	if err != nil || line != "finished\n" {
		t.Fatalf("caller exit killed sandbox: %q %v", line, err)
	}
	if err := p.WaitErr(); err != nil {
		t.Fatal(err)
	}
}

type sandboxCancelOnDeadline struct {
	context.Context
	cancel context.CancelFunc
	calls  int
}

func (c *sandboxCancelOnDeadline) Deadline() (time.Time, bool) {
	c.calls++
	if c.calls == 2 {
		c.cancel()
	}
	return c.Context.Deadline()
}
func TestSandboxPartialStartupOwnership(t *testing.T) {
	if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") != "1" && os.Getenv("SRE_TEST_BUN_RUNTIME") != "1" {
		t.Skip("opt in")
	}
	discovery, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cfg, err := discoverSandbox(discovery)
	if err != nil {
		if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	fixture := filepath.Join(t.TempDir(), "fixture.js")
	if err := os.WriteFile(fixture, []byte(`setInterval(()=>{},1000)`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"canceled-handoff", "exec-failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var launchCtx context.Context = ctx
			config := cfg
			if mode == "canceled-handoff" {
				launchCtx = &sandboxCancelOnDeadline{Context: ctx, cancel: cancel}
			} else {
				config.systemdRun = filepath.Join(t.TempDir(), "missing-systemd-run")
			}
			p, err := launchSandbox(launchCtx, config, fixture, fixture, "")
			if p != nil {
				t.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					if err := p.Close(cleanup); err != nil {
						t.Error(err)
					}
				})
			}
			if err == nil || p == nil {
				t.Fatalf("startup ownership lost: process=%v error=%v", p != nil, err)
			}
			if mode == "canceled-handoff" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			p.Abort()
			p.Abort()
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := p.Close(cleanup); err != nil {
				t.Fatal(err)
			}
			select {
			case <-p.Done:
			default:
				t.Fatal("Close did not wait for starter")
			}
		})
	}
}

func TestSandboxCanceledStartupDescriptors(t *testing.T) {
	if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") != "1" && os.Getenv("SRE_TEST_BUN_RUNTIME") != "1" {
		t.Skip("opt in")
	}
	discovery, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cfg, err := discoverSandbox(discovery)
	if err != nil {
		if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	fixture := filepath.Join(t.TempDir(), "fixture.js")
	if err := os.WriteFile(fixture, []byte(`setInterval(()=>{},1000)`), 0600); err != nil {
		t.Fatal(err)
	}
	partials := 0
	for i := 0; i < 24; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		launchCtx := &sandboxCancelOnDeadline{Context: ctx, cancel: func() { time.AfterFunc(time.Duration(i%4)*50*time.Microsecond, cancel) }}
		p, launchErr := launchSandbox(launchCtx, cfg, fixture, fixture, "")
		cancel()
		if p == nil {
			t.Fatalf("ownership not transferred: %v", launchErr)
		}
		if launchErr != nil {
			if !errors.Is(launchErr, context.Canceled) {
				t.Fatal(launchErr)
			}
			partials++
		}
		// Reuse available descriptor numbers while a canceled starter may still
		// be completing fork/exec; its child-side descriptors must remain owned.
		for j := 0; j < 32; j++ {
			f, err := os.Open("/dev/null")
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
		err := p.Close(cleanup)
		cancelCleanup()
		if err != nil {
			t.Fatal(err)
		}
		check, cancelCheck := context.WithTimeout(context.Background(), time.Second)
		output, err := sandboxOutput(check, cfg.systemctl, cfg.managerEnv, "--user", "show", p.unit, "--property=ControlGroup", "--value")
		cancelCheck()
		if err != nil || strings.TrimSpace(string(output)) != "" {
			t.Fatalf("canceled startup retained scope: %q %v", output, err)
		}
	}
	if partials == 0 {
		t.Fatal("no canceled startup handoff exercised")
	}
	t.Logf("24 canceled launches cleaned; %d returned partial handles with cancellation errors", partials)
}
