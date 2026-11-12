package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestProcessLogCLI(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "first", "--id", "one", "--session-id", "session-one")
	e := fullHook(root, "allowed", "PreToolUse", "Bash", map[string]any{"command": "kubectl get pods"})
	if code, err := invokeHook(t, "pre", e); code != 0 || err != "" {
		t.Fatal(code, err)
	}
	e = fullHook(root, "blocked", "PreToolUse", "Bash", map[string]any{"command": "kubectl delete pods"})
	invokeHook(t, "pre", e)
	for _, format := range []string{"json", "text"} {
		var out, stderr bytes.Buffer
		code := RunWithInput([]string{"log", "one", "--repo", root, "--format", format, "--by-investigator"}, strings.NewReader(""), &out, &stderr)
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("%d %s %s", code, &out, &stderr)
		}
		if format == "json" {
			var envelope map[string]any
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			data := envelope["data"].(map[string]any)
			if len(data["calls"].([]any)) != 2 || len(data["investigators"].([]any)) != 1 {
				t.Fatal(data)
			}
		} else {
			for _, label := range []string{"guard=allowed", "guard=blocked", "outcome=unknown", "exit_status=unknown", "duration_ms=unknown"} {
				if !strings.Contains(out.String(), label) {
					t.Fatalf("missing %s: %s", label, &out)
				}
			}
		}
	}
	for _, args := range [][]string{{"log", "one", "--blocked"}, {"log", "one", "--slowest", "1"}, {"--blocked=true", "log", "one"}} {
		code, envelope, stderr := ledgerCLI(t, root, "", args...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v: %d %v %q", args, code, envelope, stderr)
		}
		if len(envelope["data"].(map[string]any)["calls"].([]any)) != 1 {
			t.Fatal(envelope)
		}
	}
}
func TestProcessLogCLIRejectsInvalidArguments(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "first", "--id", "one")
	for _, args := range [][]string{
		{"log"}, {"log", "one", "extra"}, {"log", "missing"}, {"log", "../invalid"}, {"log", "one", "--slowest", "0"}, {"log", "one", "--slowest", "-1"}, {"log", "one", "--slowest", "abc"}, {"log", "one", "--slowest"}, {"log", "one", "--blocked=maybe"}, {"log", "one", "--by-investigator=maybe"}, {"version", "--blocked"}, {"run", "show", "one", "--slowest", "1"}, {"log", "one", "--namespace", "x"}, {"log", "one", "--run", "one"}, {"log", "one", "--context", "x"},
	} {
		code, envelope, _ := ledgerCLI(t, root, "", args...)
		if code != 1 || envelope["ok"] != false {
			t.Fatalf("%v: %d %v", args, code, envelope)
		}
	}
}

func TestProcessLogCLIIdentifiesUnknownRun(t *testing.T) {
	root := ledgerRepo(t)
	code, envelope, _ := ledgerCLI(t, root, "", "log", "missing")
	if code != 1 || !strings.Contains(envelope["errors"].([]any)[0].(string), "unknown run") {
		t.Fatalf("%d %v", code, envelope)
	}
}
