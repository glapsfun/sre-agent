package coderuntime

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/collect"
	"github.com/glapsfun/sre-agent/internal/sources"
)

func actualSession(t *testing.T) (context.Context, collect.Session) {
	t.Helper()
	if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") != "1" && os.Getenv("SRE_TEST_BUN_RUNTIME") != "1" {
		t.Skip("opt in to Bun runtime")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	r := New()
	if err := r.Probe(ctx); err != nil {
		if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	s, err := r.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.Close(clean); err != nil {
			t.Error(err)
		}
	})
	return ctx, s
}

func TestActualRuntimeNativeFetchHelperJoin(t *testing.T) {
	ctx, s := actualSession(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"native":true}`)) }))
	defer server.Close()
	program := `if(collection.phase==="join") return collection.values;
 if(!Array.isArray(collection.values)||collection.values.length!==0||collection.keys.length!==1||collection.keys[0].source_id!==collection.source_id||"program" in collection||"host_port" in collection||"arguments" in collection.keys[0]) throw new Error();
 const native=await (await fetch(` + strconvQuote(server.URL) + `)).json();
 const helper=await collection.call(collection.keys[0].key);
 return [{key:collection.keys[0].key,source_id:collection.source_id,data:{native,helper}}];`
	values := []collect.Value{}
	for _, id := range []string{"one", "two"} {
		inv := collect.Invocation{Phase: "source", SourceID: id, HostPort: strings.TrimPrefix(server.URL, "http://"), Program: program, Keys: []collect.Key{{Key: id, SourceID: id, Tool: "read"}}}
		got, err := s.Run(ctx, inv, func(key string) (sources.CallResult, error) {
			return sources.CallResult{SourceID: id, Data: json.RawMessage(`{"helper":true}`)}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Values) != 1 || !strings.Contains(string(got.Values[0].Data), `"ok":true`) || len(got.Audit) != 1 || got.Audit[0].SourceID != id {
			t.Fatalf("execution=%+v", got)
		}
		values = append(values, got.Values...)
	}
	got, err := s.Run(ctx, collect.Invocation{Phase: "join", Program: program, Values: values}, nil)
	if err != nil || len(got.Values) != 2 || len(got.Audit) != 0 {
		t.Fatalf("join=%+v err=%v", got, err)
	}
}
func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestActualRuntimeTLS(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"tls":true}`) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	defer server.Close()
	for _, hostname := range []bool{false, true} {
		t.Run(fmt.Sprint(hostname), func(t *testing.T) {
			ctx, s := actualSession(t)
			target := strings.TrimPrefix(server.URL, "https://")
			if hostname {
				target = strings.Replace(target, "127.0.0.1", "localhost", 1)
			}
			program := `const response=await fetch("https://` + target + `",{tls:{ca:` + strconvQuote(ca) + `}});const data=await response.json();await collection.call("own");return [{key:"own",source_id:"s",data}];`
			var helpers atomic.Int32
			got, err := s.Run(ctx, collect.Invocation{Phase: "source", SourceID: "s", HostPort: target, Keys: []collect.Key{{Key: "own", SourceID: "s", Tool: "read"}}, Program: program}, func(string) (sources.CallResult, error) { helpers.Add(1); return sources.CallResult{}, nil })
			if err != nil || len(got.Values) != 1 || string(got.Values[0].Data) != `{"tls":true}` || len(got.Audit) != 1 || got.Audit[0].Value != target {
				t.Fatalf("TLS execution=%+v err=%v helpers=%d", got, err, helpers.Load())
			}
		})
	}
}

func TestActualRuntimeDeniedNetwork(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			ctx, s := actualSession(t)
			var forbiddenCalls atomic.Int32
			forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forbiddenCalls.Add(1) }))
			defer forbidden.Close()
			allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, forbidden.URL, 302) }))
			defer allowed.Close()
			url := forbidden.URL
			if redirect {
				url = allowed.URL
			}
			program := `try{await fetch(` + strconvQuote(url) + `)}catch{} return [];`
			got, err := s.Run(ctx, collect.Invocation{Phase: "source", SourceID: "s", HostPort: strings.TrimPrefix(allowed.URL, "http://"), Keys: []collect.Key{{Key: "own", SourceID: "s", Tool: "read"}}, Program: program}, func(string) (sources.CallResult, error) { return sources.CallResult{}, nil })
			if err == nil || forbiddenCalls.Load() != 0 {
				t.Fatalf("err=%v forbidden=%d", err, forbiddenCalls.Load())
			}
			denied := false
			for _, a := range got.Audit {
				if a.Phase != "source" || a.SourceID != "s" {
					t.Fatal(a)
				}
				denied = denied || a.Permission == "network-denied"
			}
			if !denied {
				t.Fatalf("missing denial audit: %+v", got)
			}
			encoded, _ := json.Marshal(got)
			time.Sleep(20 * time.Millisecond)
			after, _ := json.Marshal(got)
			if string(encoded) != string(after) {
				t.Fatal("audit mutated after return")
			}
		})
	}
}

func TestActualRuntimeNetworkIsolation(t *testing.T) {
	ctx, s := actualSession(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	target := strings.TrimPrefix(server.URL, "http://")
	_, port, _ := net.SplitHostPort(target)
	program := `const net=await import("node:net");const fs=await import("node:fs");
 const reached=await new Promise(resolve=>{const s=net.connect({host:"127.0.0.1",port:` + port + `});s.on("connect",()=>{s.destroy();resolve(true)});s.on("error",()=>resolve(false));s.setTimeout(100,()=>{s.destroy();resolve(false)});});
 return [{key:"own",source_id:"s",data:{reached,proxy:collection.proxy_url,socket:fs.existsSync("/run/egress.sock")}}];`
	inv := collect.Invocation{Phase: "source", SourceID: "s", HostPort: target, Keys: []collect.Key{{Key: "own", SourceID: "s", Tool: "read"}}, Program: program}
	got, err := s.Run(ctx, inv, func(string) (sources.CallResult, error) { return sources.CallResult{}, nil })
	if err != nil || len(got.Values) != 1 || strings.Contains(string(got.Values[0].Data), `"reached":true`) {
		t.Fatalf("source=%+v err=%v", got, err)
	}
	got, err = s.Run(ctx, collect.Invocation{Phase: "join", Program: program, Values: got.Values}, nil)
	if err != nil || len(got.Values) != 1 || string(got.Values[0].Data) != `{"reached":false,"proxy":null,"socket":false}` || calls.Load() != 0 {
		t.Fatalf("join=%+v err=%v contacts=%d", got, err, calls.Load())
	}
}

func TestActualRuntimeFailuresAndCleanup(t *testing.T) {
	for _, tc := range []struct{ name, program string }{
		{"exception", `throw new Error("private-program-secret");`},
		{"console", `console.log("untrusted");return [];`},
		{"stdout", `process.stdout.write("x".repeat(100000));await new Promise(()=>{});`},
		{"stderr", `process.stderr.write("private-stderr-secret".repeat(10000));await new Promise(()=>{});`},
		{"cpu", `while(true){}`},
		{"memory", `const a=[];while(true)a.push(Buffer.alloc(16*1024*1024,7));`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, session := actualSession(t)
			s := session.(*runtimeSession)
			got, err := s.Run(ctx, collect.Invocation{Phase: "join", Program: tc.program}, nil)
			if err == nil || strings.Contains(err.Error(), "private-") || len(got.Audit) != 0 {
				t.Fatalf("execution=%+v error=%v", got, err)
			}
			if _, err := s.Run(ctx, collect.Invocation{Phase: "join", Program: "return [];"}, nil); err == nil {
				t.Fatal("failed session reused")
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := s.Close(cleanup); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
				t.Fatalf("temporary state retained: %v", err)
			}
			for _, p := range s.processes {
				select {
				case <-p.Done:
				default:
					t.Fatal("child not reaped")
				}
				if p.cgroup != "" {
					if data, err := os.ReadFile(filepath.Join(p.cgroup, "cgroup.procs")); err == nil && len(strings.TrimSpace(string(data))) != 0 {
						t.Fatalf("live scope: %s", data)
					}
				}
			}
		})
	}
}

func TestActualRuntimeCancellationAndClose(t *testing.T) {
	ctx, session := actualSession(t)
	s := session.(*runtimeSession)
	running := make(chan struct{})
	result := make(chan error, 1)
	inv := collect.Invocation{Phase: "source", SourceID: "s", HostPort: "127.0.0.1:1", Keys: []collect.Key{{Key: "own", SourceID: "s", Tool: "read"}}, Program: `await collection.call("own");return [];`}
	phase, cancel := context.WithCancel(ctx)
	go func() {
		_, err := s.Run(phase, inv, func(string) (sources.CallResult, error) {
			close(running)
			<-phase.Done()
			return sources.CallResult{}, phase.Err()
		})
		result <- err
	}()
	select {
	case <-running:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := s.Run(ctx, collect.Invocation{Phase: "join", Program: "return [];"}, nil); err == nil {
		t.Fatal("overlap accepted")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	clean, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := s.Close(clean); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(clean); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, inv, nil); err == nil {
		t.Fatal("closed session reused")
	}
}

func TestActualRuntimeCollectNeverFallsBack(t *testing.T) {
	ctx, _ := actualSession(t)
	var contacts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacts.Add(1)
		http.Error(w, "unexpected fallback", 500)
	}))
	defer server.Close()
	root := t.TempDir()
	src := sources.Source{ID: "fixture", Name: "fixture", Kind: "utcp-http", Endpoint: server.URL + "/manual", CredentialEnv: "FIXTURE_TOKEN", AccessRef: "test-reader", CodeNetwork: &sources.CodeNetwork{HostPort: strings.TrimPrefix(server.URL, "http://"), AccessRef: "test-reader-network"}, Tools: []sources.Tool{{Name: "read", ReadOnly: true, SchemaDigest: strings.Repeat("a", 64)}}}
	if _, err := sources.Approve(ctx, root, sources.ApprovalRequest{Approved: true, CodeNetworkApproved: true, Sources: []sources.Source{src}}); err != nil {
		t.Fatal(err)
	}
	service := collect.Service{Runtime: New(), Sources: sources.Service{LookupEnv: func(string) (string, bool) { return "private-test-token", true }}}
	plan := collect.Plan{Operation: "filter", Required: []string{"one", "two"}, Calls: []collect.Call{{Key: "one", SourceID: "fixture", Tool: "read", Arguments: json.RawMessage(`{}`)}, {Key: "two", SourceID: "fixture", Tool: "read", Arguments: json.RawMessage(`{}`)}}}
	got, err := service.Collect(ctx, root, collect.Request{Mode: collect.ModeAuto, Plan: plan, Program: `throw new Error("private-test-token");`, CodeApproved: true})
	if !errors.Is(err, collect.ErrIncompleteCollection) || got.SelectedMode != collect.ModeCode || contacts.Load() != 0 || len(got.Activity) != 0 || got.RequiredKeysSatisfied {
		t.Fatalf("collection=%+v err=%v contacts=%d", got, err, contacts.Load())
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "private-test-token") {
		t.Fatal("secret escaped into result")
	}
}

func unrelatedProbeScope(t *testing.T) {
	t.Helper()
	ctx, session := actualSession(t)
	phase, cancel := context.WithCancel(ctx)
	entered, done := make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := session.Run(phase, collect.Invocation{Phase: "source", SourceID: "s", HostPort: "127.0.0.1:1", Keys: []collect.Key{{Key: "own", SourceID: "s", Tool: "read"}}, Program: `await collection.call("own");return [];`}, func(string) (sources.CallResult, error) {
			close(entered)
			<-phase.Done()
			return sources.CallResult{}, phase.Err()
		})
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("unrelated scope cancellation: %v", err)
		}
	})
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func recordProbeUnit(t *testing.T, root string) string {
	t.Helper()
	launcher, err := exec.LookPath("systemd-run")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "probe-unit")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nprevious=\nfor arg in \"$@\"; do\n if [ \"$previous\" = --unit ]; then printf '%s\\n' \"$arg\" > " + quote(path) + "; break; fi\n previous=$arg\ndone\nexec " + quote(launcher) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(root, "systemd-run"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func probeScopePath(unitFile string) (string, error) {
	raw, err := os.ReadFile(unitFile)
	if err != nil {
		return "", err
	}
	unit := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(unit, "sre-bun-") || !strings.HasSuffix(unit, ".scope") || filepath.Base(unit) != unit {
		return "", errors.New("invalid recorded Probe unit")
	}
	return filepath.Join(fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/app.slice", os.Getuid(), os.Getuid()), unit, "cgroup.procs"), nil
}

func TestActualCanceledProbeState(t *testing.T) {
	if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") != "1" && os.Getenv("SRE_TEST_BUN_RUNTIME") != "1" {
		t.Skip("opt in")
	}
	unrelatedProbeScope(t)
	root := t.TempDir()
	unitFile := recordProbeUnit(t, root)
	t.Setenv("TMPDIR", root)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watched := make(chan struct{})
	ownedScope := ""
	go func() {
		defer close(watched)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				path, err := probeScopePath(unitFile)
				if err != nil {
					continue
				}
				if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
					ownedScope = path
					cancel()
					return
				}
			}
		}
	}()
	err := New().Probe(ctx)
	<-watched
	if !errors.Is(err, context.Canceled) || !errors.Is(err, collect.ErrCodeUnavailable) {
		t.Fatalf("probe cancellation=%v", err)
	}
	if ownedScope == "" {
		t.Fatal("tested Probe never entered its own live scope")
	}
	paths, _ := filepath.Glob(filepath.Join(root, "sre-bun-*"))
	t.Logf("canceled Probe retained temporary directories=%d; success was not reported", len(paths))
	deadline := time.Now().Add(time.Second)
	for {
		data, readErr := os.ReadFile(ownedScope)
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			t.Log("canceled Probe has no live process in its owned scope")
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled Probe retained live scope")
		}
		time.Sleep(time.Millisecond)
	}
	if len(paths) != 0 {
		t.Fatalf("canceled Probe retained %d temporary directories", len(paths))
	}
}

func TestActualRuntimePartialLaunchOwnership(t *testing.T) {
	ctx, session := actualSession(t)
	s := session.(*runtimeSession)
	s.cfg.systemdRun = filepath.Join(t.TempDir(), "missing-launcher")
	if _, err := s.Run(ctx, collect.Invocation{Phase: "join", Program: "return [];"}, nil); err == nil {
		t.Fatal("missing launcher succeeded")
	}
	if len(s.processes) != 1 {
		t.Fatalf("partial ownership lost: %d", len(s.processes))
	}
	clean, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(clean); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.processes[0].Done:
	default:
		t.Fatal("starter not reaped")
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("temporary state retained: %v", err)
	}
}

func TestActualRuntimeCanceledBeforeRun(t *testing.T) {
	ctx, s := actualSession(t)
	phase, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Run(phase, collect.Invocation{Phase: "join", Program: "return [];"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestActualRuntimeHelperQueueAndOwnership(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(fmt.Sprint(forged), func(t *testing.T) {
			ctx, s := actualSession(t)
			var calls, active, maximum atomic.Int32
			program := `const results=await Promise.all(Array.from({length:5},()=>collection.call("own")));return [{key:"own",source_id:"s",data:results.map(x=>x.ok)}];`
			if forged {
				program = `collection.keys.push({key:"other",source_id:"other",tool:"read"});await collection.call("other");return [];`
			}
			got, err := s.Run(ctx, collect.Invocation{Phase: "source", SourceID: "s", HostPort: "127.0.0.1:1", Program: program, Keys: []collect.Key{{Key: "own", SourceID: "s", Tool: "read"}}}, func(string) (sources.CallResult, error) {
				calls.Add(1)
				n := active.Add(1)
				for {
					old := maximum.Load()
					if n <= old || maximum.CompareAndSwap(old, n) {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
				active.Add(-1)
				return sources.CallResult{Data: json.RawMessage(`true`)}, nil
			})
			if forged {
				if err == nil || calls.Load() != 0 {
					t.Fatalf("expanded authority err=%v calls=%d", err, calls.Load())
				}
				return
			}
			if err != nil || len(got.Values) != 1 || string(got.Values[0].Data) != `[true,true,true,true,true]` || calls.Load() != 5 || maximum.Load() > 2 {
				t.Fatalf("execution=%+v err=%v calls=%d active=%d", got, err, calls.Load(), maximum.Load())
			}
		})
	}
}

func TestActualProbeReservedExecutionDeadline(t *testing.T) {
	if os.Getenv("SRE_REQUIRE_BUN_RUNTIME") != "1" && os.Getenv("SRE_TEST_BUN_RUNTIME") != "1" {
		t.Skip("opt in")
	}
	unrelatedProbeScope(t)
	root := t.TempDir()
	unitFile := recordProbeUnit(t, root)
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "systemctl")
	script := "#!/bin/sh\ncase \"$3\" in sre-bun-*.scope) if [ \"$4\" = \"--property=ControlGroup\" ]; then exec /usr/bin/sleep 2; fi;; esac\nexec '" + strings.ReplaceAll(systemctl, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", root)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = New().Probe(ctx)
	if !errors.Is(err, collect.ErrCodeUnavailable) || ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reserved deadline error=%v caller=%v", err, ctx.Err())
	}
	ownedScope, readErr := probeScopePath(unitFile)
	if readErr != nil {
		t.Fatalf("tested Probe never launched its own scope: %v", readErr)
	}
	paths, _ := filepath.Glob(filepath.Join(root, "sre-bun-*"))
	if len(paths) != 0 {
		t.Fatalf("reserved deadline retained %d directories", len(paths))
	}
	data, readErr := os.ReadFile(ownedScope)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(strings.TrimSpace(string(data))) != 0 {
		t.Fatalf("reserved deadline retained live scope %s", data)
	}
	t.Log("reserved execution deadline expired; caller remains live, private directories removed, no live owned scope")
}

func TestActualRuntimePendingFinalDuringReply(t *testing.T) {
	ctx, s := actualSession(t)
	secondEntered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	program := `process.stdin.once("data",()=>{
 process.stdin.pause();process.stdout.write('{"type":"final","values":[]}\n');
});
process.stdout.write('{"type":"call","id":"a","key":"own"}\n{"type":"call","id":"b","key":"own"}\n');
await new Promise(()=>{});`
	start := time.Now()
	_, err := s.Run(ctx, collect.Invocation{Phase: "source", SourceID: "s", HostPort: "127.0.0.1:1", Program: program, Keys: []collect.Key{{Key: "own", SourceID: "s", Tool: "read"}}}, func(string) (sources.CallResult, error) {
		if calls.Add(1) == 1 {
			<-secondEntered
			return sources.CallResult{Data: json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)}, nil
		}
		close(secondEntered)
		<-release
		return sources.CallResult{Data: json.RawMessage(`true`)}, nil
	})
	if err == nil || ctx.Err() != nil || calls.Load() != 2 || time.Since(start) > 2*time.Second {
		t.Fatalf("pending final rejection: err=%v caller=%v calls=%d elapsed=%s", err, ctx.Err(), calls.Load(), time.Since(start))
	}
}
