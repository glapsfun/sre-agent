package cli

import (
	"bytes"
	"strings"
	"testing"
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
