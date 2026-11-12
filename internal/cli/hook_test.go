package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/glapsfun/sre-agent/internal/processlog"
	"github.com/glapsfun/sre-agent/internal/store"
)

func TestHookPre(t *testing.T) {
	for _, tc := range []struct {
		name, input, reason string
		code                int
	}{
		{"allow", `{"agent_type":"sre-k8s-investigator","tool_name":"Bash","tool_input":{"command":"kubectl get pods"}}`, "", 0},
		{"deny", `{"agent_type":"sre-k8s-investigator","tool_name":"Bash","tool_input":{"command":"kubectl delete pods"}}`, "kubectl delete pods isn't a read-only command", 2},
		{"main malformed details", `{"tool_name":{},"tool_input":42}`, "", 0},
		{"other malformed details", `{"agent_type":"general-purpose","tool_input":42}`, "", 0},
		{"broken investigator", `{"agent_type":"sre-k8s-investigator", broken`, "the guard couldn't read the hook event", 2},
		{"broken prefixed investigator", `{"agent_type":"plugin\\prefix:sre-k8s-investigator", broken`, "the guard couldn't read the hook event", 2},
		{"broken investigator whitespace", "{\"agent_type\"\v:\"sre-k8s-investigator\", broken", "the guard couldn't read the hook event", 2},
		{"broken main", `{"tool_input": broken`, "", 0},
		{"missing command", `{"agent_type":"sre-k8s-investigator","tool_name":"Bash"}`, "the guard couldn't read the Bash command", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := RunWithInput([]string{"hook", "pre"}, strings.NewReader(tc.input), &out, &stderr)
			want := ""
			if tc.reason != "" {
				want = "sre-agent read-only guard blocked this: " + tc.reason + ". Record it under GAPS and move on. Don't look for another way to run it.\n"
			}
			if code != tc.code || out.Len() != 0 || stderr.String() != want {
				t.Fatalf("exit %d stdout %q stderr %q, want exit %d stderr %q", code, out.String(), stderr.String(), tc.code, want)
			}
		})
	}
}

func fullHook(root, id, event, tool string, input any) map[string]any {
	return map[string]any{"session_id": "session-one", "transcript_path": "/unused/transcript.jsonl", "cwd": root, "permission_mode": "default", "hook_event_name": event, "tool_name": tool, "tool_input": input, "tool_use_id": id, "agent_type": "sre-agent:sre-k8s-investigator"}
}
func invokeHook(t *testing.T, phase string, payload map[string]any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := RunWithInput([]string{"hook", phase}, bytes.NewReader(raw), &out, &stderr)
	if out.Len() != 0 {
		t.Fatalf("hook stdout: %s", &out)
	}
	return code, stderr.String()
}
func hookReport(t *testing.T, root, run string) processlog.Report {
	t.Helper()
	s, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := processlog.Query(t.Context(), s, run, processlog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestHookSessionIntegration(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "hook session", "--id", "run-one", "--session-id", "session-one")
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	investigators := []string{"sre-k8s-investigator", "sre-metrics-analyst", "sre-logs-investigator", "sre-change-historian", "sre-trace-analyst", "sre-eks-investigator", "sre-gke-investigator"}
	for i, agent := range investigators {
		e := fullHook(nested, fmt.Sprintf("call-%d", i), "PreToolUse", "Bash", map[string]any{"command": "kubectl get pods"})
		e["agent_type"] = "sre-agent:" + agent
		for range 2 {
			if code, stderr := invokeHook(t, "pre", e); code != 0 || stderr != "" {
				t.Fatalf("pre %d %q", code, stderr)
			}
		}
		if i == 0 {
			continue
		}
		phase := "PostToolUse"
		if i%2 == 0 {
			phase = "PostToolUseFailure"
		}
		e["hook_event_name"] = phase
		if i%2 == 0 {
			e["error"] = "Exit code 7\nplanted-failure-content"
			e["is_interrupt"] = false
		} else {
			e["tool_response"] = map[string]any{"stdout": "planted-output-content", "stderr": "", "interrupted": false, "isImage": false}
		}
		if i == 1 {
			e["duration_ms"] = 12
		}
		for range 2 {
			if code, stderr := invokeHook(t, "post", e); code != 0 || stderr != "" {
				t.Fatalf("post %d %q", code, stderr)
			}
		}
	}
	for _, tc := range []struct {
		id, tool string
		input    any
		blocked  bool
	}{
		{"blocked", "Bash", map[string]any{"command": "kubectl delete pods"}, true},
		{"read", "Read", map[string]any{"file_path": "/repo/README.md"}, false},
		{"grep", "Grep", map[string]any{"path": "/repo/src", "pattern": "planted-query-content"}, false},
		{"mcp", "mcp__plugin_sre-agent_sre__prom_query", map[string]any{"query": "planted-query-content"}, false},
		{"mcp-write", "mcp__plugin_sre-agent_sre__ledger_add", map[string]any{"payload": "planted-payload-content"}, true},
	} {
		e := fullHook(nested, tc.id, "PreToolUse", tc.tool, tc.input)
		code, stderr := invokeHook(t, "pre", e)
		want := 0
		if tc.blocked {
			want = 2
		}
		if code != want || !tc.blocked && stderr != "" {
			t.Fatalf("%s pre: %d %q", tc.id, code, stderr)
		}
		if !tc.blocked {
			e["hook_event_name"] = "PostToolUse"
			e["tool_response"] = map[string]any{"content": "planted-response-content"}
			if code, stderr := invokeHook(t, "post", e); code != 0 || stderr != "" {
				t.Fatalf("post: %d %q", code, stderr)
			}
		}
	}
	r := hookReport(t, root, "run-one")
	if len(r.Calls) != 12 {
		t.Fatalf("one row per call: %d %+v", len(r.Calls), r)
	}
	byID := map[string]processlog.Call{}
	for _, c := range r.Calls {
		byID[c.ToolUseID] = c
		if c.ExitStatus != nil {
			t.Fatal("invented status", c)
		}
	}
	if byID["call-0"].Outcome != "unknown" || *byID["call-0"].GuardDecision != "allowed" || byID["call-0"].DurationMS != nil {
		t.Fatal(byID["call-0"])
	}
	if byID["call-1"].Outcome != "succeeded" || byID["call-1"].DurationMS == nil || *byID["call-1"].DurationMS != 12 || byID["call-2"].Outcome != "failed" || byID["call-2"].DurationMS != nil {
		t.Fatal(r)
	}
	if byID["blocked"].Outcome != "unknown" || *byID["blocked"].GuardDecision != "blocked" || byID["mcp"].Outcome != "succeeded" || byID["read"].Path != "/repo/README.md" || byID["grep"].Path != "/repo/src" {
		t.Fatal(r)
	}
	s, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB().QueryRow("SELECT count(*) FROM commands").Scan(&count); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if count != 12 {
		t.Fatalf("SQL row count %d", count)
	}
	files, _ := filepath.Glob(filepath.Join(root, ".sre-agent", "sre.db*"))
	encoded, _ := json.Marshal(r)
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, b...)
	}
	if strings.Contains(string(encoded), "planted-") {
		t.Fatal("raw response/input/error persisted")
	}
	if _, err := os.Stat(filepath.Join(nested, ".sre-agent")); !os.IsNotExist(err) {
		t.Fatalf("nested store: %v", err)
	}
}
func TestHookRunAssociationAndSessionIsolation(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "first", "--id", "one", "--session-id", "session-one")
	ledgerData(t, root, "", "run", "start", "latest unrelated", "--id", "two", "--session-id", "session-two")
	e := fullHook(root, "shared", "PreToolUse", "Bash", map[string]any{"command": "kubectl get pods"})
	if code, err := invokeHook(t, "pre", e); code != 0 || err != "" {
		t.Fatal(code, err)
	}
	e["session_id"] = "session-two"
	if code, err := invokeHook(t, "pre", e); code != 0 || err != "" {
		t.Fatal(code, err)
	}
	if len(hookReport(t, root, "one").Calls) != 1 || len(hookReport(t, root, "two").Calls) != 1 {
		t.Fatal("sessions crossed")
	}
	ledgerData(t, root, `{"session_id":"session-one"}`, "ledger", "add", "metadata", "--run", "two", "--id", "rebind", "--file", "-")
	e["session_id"] = "session-one"
	e["hook_event_name"] = "PostToolUse"
	if code, err := invokeHook(t, "post", e); code != 0 || !strings.Contains(err, "conflict") {
		t.Fatal(code, err)
	}
	if r := hookReport(t, root, "one"); len(r.Calls) != 1 || r.Calls[0].Outcome != "unknown" {
		t.Fatal(r)
	}
	t.Setenv("SRE_RUN_ID", "one")
	if code, err := invokeHook(t, "post", e); code != 0 || err != "" {
		t.Fatal(code, err)
	}
	if r := hookReport(t, root, "one"); r.Calls[0].Outcome != "succeeded" {
		t.Fatal(r)
	}
	for _, id := range []string{"missing", "../invalid"} {
		t.Setenv("SRE_RUN_ID", id)
		e["tool_use_id"] = "new"
		if code, err := invokeHook(t, "post", e); code != 0 || !strings.Contains(err, "association") {
			t.Fatal(code, err)
		}
	}
	t.Setenv("SRE_RUN_ID", "")
	e["session_id"] = "unknown-session"
	if code, err := invokeHook(t, "post", e); code != 0 || !strings.Contains(err, "association") {
		t.Fatal(code, err)
	}
	if len(hookReport(t, root, "two").Calls) != 1 {
		t.Fatal("fell back to latest")
	}
}
func TestHookErrorsNeverChangeGuardDecisions(t *testing.T) {
	root := ledgerRepo(t)
	if err := os.WriteFile(filepath.Join(root, ".sre-agent"), []byte("unusable store"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"pre", "post"} {
		for _, agent := range []string{"", "general-purpose", "sre-k8s-investigator"} {
			for _, blocked := range []bool{false, true} {
				command := "kubectl get pods"
				if blocked {
					command = "kubectl delete pods"
				}
				name := "PreToolUse"
				if phase == "post" {
					name = "PostToolUseFailure"
				}
				e := fullHook(root, "a", name, "Bash", map[string]any{"command": command})
				e["agent_type"] = agent
				code, stderr := invokeHook(t, phase, e)
				want := 0
				if phase == "pre" && blocked && agent == "sre-k8s-investigator" {
					want = 2
				}
				if code != want {
					t.Fatalf("%s %s: %d %q", phase, agent, code, stderr)
				}
				if agent == "sre-k8s-investigator" && !strings.Contains(stderr, "process log") {
					t.Fatalf("missing diagnostic: %q", stderr)
				}
				if agent != "sre-k8s-investigator" && stderr != "" {
					t.Fatalf("unrelated diagnostic %q", stderr)
				}
			}
		}
	}
}
func TestHookMalformedMetadataAndPostInput(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "first", "--id", "one", "--session-id", "session-one")
	for _, field := range []string{"session_id", "tool_use_id", "cwd", "hook_event_name"} {
		for _, bad := range []any{nil, "", 42} {
			e := fullHook(root, "bad", "PreToolUse", "Bash", map[string]any{"command": "kubectl get pods"})
			e[field] = bad
			if code, err := invokeHook(t, "pre", e); code != 0 || !strings.Contains(err, "process log") {
				t.Fatalf("%s %v: %d %q", field, bad, code, err)
			}
		}
	}
	for _, duration := range []any{"12", -1, 1.5} {
		e := fullHook(root, "bad", "PostToolUse", "Bash", map[string]any{"command": "kubectl get pods"})
		e["duration_ms"] = duration
		if code, err := invokeHook(t, "post", e); code != 0 || !strings.Contains(err, "process log") {
			t.Fatalf("duration %v: %d %q", duration, code, err)
		}
	}
	for _, input := range []string{`{"agent_type":"sre-k8s-investigator",broken`, `null`} {
		var out, err bytes.Buffer
		if code := RunWithInput([]string{"hook", "post"}, strings.NewReader(input), &out, &err); code != 0 || out.Len() != 0 {
			t.Fatal(code, &out, &err)
		}
	}
	if r := hookReport(t, root, "one"); len(r.Calls) != 0 {
		t.Fatal("invalid metadata persisted", r)
	}
}

func TestConfiguredHookCommands(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "configured session", "--id", "one", "--session-id", "session-one")
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Command string }
		}
	}
	raw, err := os.ReadFile(filepath.Join(source, "plugins/sre-agent/hooks/hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"PreToolUse", "PostToolUse", "PostToolUseFailure"} {
		entries := config.Hooks[event]
		if len(entries) != 1 || len(entries[0].Hooks) != 1 {
			t.Fatalf("missing configured %s command", event)
		}
		matcher, err := regexp.Compile("^(?:" + entries[0].Matcher + ")$")
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range []string{"Bash", "Read", "Grep", "mcp__plugin_sre-agent_sre__prom_query", "mcp__plugin_sre-agent_sre__ledger_add"} {
			if !matcher.MatchString(tool) {
				t.Fatalf("%s misses %s", event, tool)
			}
		}
	}
	launcher, err := os.ReadFile(filepath.Join(source, "plugins/sre-agent/skills/sre-agent/scripts/sre"))
	if err != nil {
		t.Fatal(err)
	}
	version := ""
	for _, line := range strings.Split(string(launcher), "\n") {
		if strings.HasPrefix(line, "version=") {
			version = strings.TrimPrefix(line, "version=")
		}
	}
	if version == "" {
		t.Fatal("launcher version unavailable")
	}
	cache := t.TempDir()
	release := filepath.Join(cache, "sre-agent", version, runtime.GOOS+"-"+runtime.GOARCH)
	if err := os.MkdirAll(release, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(release, "sre")
	build := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X github.com/glapsfun/sre-agent/internal/cli.Version="+version, "-o", binary, "./cmd/sre")
	build.Dir = source
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	binaryBytes, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	checksum := fmt.Sprintf("%x  sre_%s_%s_%s\n", sha256.Sum256(binaryBytes), version, runtime.GOOS, runtime.GOARCH)
	if err := os.WriteFile(filepath.Join(release, "SHA256SUMS"), []byte(checksum), 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(event, id, tool string, blocked bool) {
		t.Helper()
		payload := fullHook(root, id, event, tool, map[string]any{"command": "kubectl get pods"})
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("bash", "-c", config.Hooks[event][0].Hooks[0].Command)
		command.Stdin = bytes.NewReader(raw)
		command.Env = append(os.Environ(), "CLAUDE_PLUGIN_ROOT="+filepath.Join(source, "plugins/sre-agent"), "CLAUDE_PLUGIN_DATA="+cache)
		var out, stderr bytes.Buffer
		command.Stdout = &out
		command.Stderr = &stderr
		err = command.Run()
		if blocked {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("blocked %v %s", err, &stderr)
			}
		} else if err != nil || stderr.Len() != 0 {
			t.Fatalf("%s: %v %s", event, err, &stderr)
		}
		if out.Len() != 0 {
			t.Fatal("hook stdout", &out)
		}
	}
	for _, tc := range []struct{ id, tool, terminal string }{{"shell", "Bash", "PostToolUse"}, {"mcp", "mcp__plugin_sre-agent_sre__prom_query", "PostToolUseFailure"}} {
		invoke("PreToolUse", tc.id, tc.tool, false)
		invoke(tc.terminal, tc.id, tc.tool, false)
		invoke(tc.terminal, tc.id, tc.tool, false)
	}
	invoke("PreToolUse", "write", "mcp__plugin_sre-agent_sre__ledger_add", true)
	r := hookReport(t, root, "one")
	if len(r.Calls) != 3 {
		t.Fatalf("configured call count: %+v", r)
	}

	// A verified checker returning 2 and an unavailable cache must not block post hooks.
	failed := []byte("#!/bin/sh\necho checker-failed >&2\nexit 2\n")
	if err := os.WriteFile(binary, failed, 0700); err != nil {
		t.Fatal(err)
	}
	checksum = fmt.Sprintf("%x  sre_%s_%s_%s\n", sha256.Sum256(failed), version, runtime.GOOS, runtime.GOARCH)
	if err := os.WriteFile(filepath.Join(release, "SHA256SUMS"), []byte(checksum), 0600); err != nil {
		t.Fatal(err)
	}
	missing := t.TempDir()
	if err := os.MkdirAll(filepath.Join(missing, "sre-agent", version, runtime.GOOS+"-"+runtime.GOARCH), 0700); err != nil {
		t.Fatal(err)
	}
	for _, testCache := range []string{cache, missing} {
		for _, event := range []string{"PostToolUse", "PostToolUseFailure"} {
			for _, agent := range []string{"", "general-purpose", "sre-k8s-investigator"} {
				payload := fullHook(root, "startup-failure", event, "Bash", map[string]any{"command": "kubectl get pods"})
				payload["agent_type"] = agent
				raw, _ := json.Marshal(payload)
				command := exec.Command("bash", "-c", config.Hooks[event][0].Hooks[0].Command)
				command.Stdin = bytes.NewReader(raw)
				command.Env = append(os.Environ(), "CLAUDE_PLUGIN_ROOT="+filepath.Join(source, "plugins/sre-agent"), "CLAUDE_PLUGIN_DATA="+testCache)
				var out, stderr bytes.Buffer
				command.Stdout = &out
				command.Stderr = &stderr
				if err := command.Run(); err != nil || out.Len() != 0 || stderr.Len() == 0 {
					t.Fatalf("post startup failure: %v stdout=%q stderr=%q", err, &out, &stderr)
				}
			}
		}
	}
}

func TestHookCredentialArgumentFragmentsAreNotReported(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "redaction regression", "--id", "one", "--session-id", "session-one")
	commands := []string{`curl -u'user:prefix planted-quoted-tail' https://example.test`, `curl -u"user:prefix planted-double-tail" https://example.test`, `curl --token prefix\ planted-escaped-tail https://example.test`}
	for i, command := range commands {
		e := fullHook(root, fmt.Sprintf("credential-%d", i), "PreToolUse", "Bash", map[string]any{"command": command})
		for range 2 {
			if code, stderr := invokeHook(t, "pre", e); code != 0 || stderr != "" {
				t.Fatalf("guard decision changed: %d %q", code, stderr)
			}
		}
		e["hook_event_name"] = "PostToolUse"
		if code, stderr := invokeHook(t, "post", e); code != 0 || stderr != "" {
			t.Fatalf("post: %d %q", code, stderr)
		}
	}
	r := hookReport(t, root, "one")
	if len(r.Calls) != 3 {
		t.Fatalf("rows: %+v", r)
	}
	for _, c := range r.Calls {
		if c.GuardDecision == nil || *c.GuardDecision != "allowed" || c.Outcome != "succeeded" {
			t.Fatal(c)
		}
	}
	for _, format := range []string{"json", "text"} {
		var out, stderr bytes.Buffer
		code := RunWithInput([]string{"log", "one", "--repo", root, "--format", format}, strings.NewReader(""), &out, &stderr)
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("query: %d %q", code, &stderr)
		}
		if strings.Contains(out.String(), "planted-") || strings.Contains(out.String(), "prefix") {
			t.Errorf("credential leaked in %s output: %s", format, &out)
		}
	}
	files, err := filepath.Glob(filepath.Join(root, ".sre-agent", "sre.db*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "planted-") || strings.Contains(string(raw), "prefix") {
			t.Errorf("credential persisted in %s", filepath.Base(file))
		}
	}
}
