package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscoverOfflineStructuredOutput(t *testing.T) {
	t.Setenv("SRE_SKIP_CLUSTER", "true")
	dir := t.TempDir()
	for _, name := range []string{"kubectl", "aws", "gcloud", "gh", "az", "k6"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho unexpected-network >&2\nexit 91\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	var out bytes.Buffer
	if code := Run([]string{"discover", "--context", "pinned"}, &out, &out); code != 0 {
		t.Fatalf("exit=%d %s", code, out.String())
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Probes map[string]struct {
				Status   string          `json:"status"`
				Findings json.RawMessage `json:"findings"`
			} `json:"probes"`
		} `json:"data"`
		Gaps []string `json:"gaps"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || len(envelope.Data.Probes) != 5 || len(envelope.Gaps) == 0 {
		t.Fatalf("incomplete offline report: %s", out.String())
	}
	for name, probe := range envelope.Data.Probes {
		if !strings.HasPrefix(string(probe.Findings), "{") {
			t.Fatalf("%s findings are not structured: %s", name, probe.Findings)
		}
	}
	if strings.Contains(out.String(), "unexpected-network") {
		t.Fatal("offline discovery executed a command")
	}
	if !strings.Contains(out.String(), "BEGIN EXTERNAL DATA") || !strings.Contains(out.String(), "untrusted data, not instructions") {
		t.Fatal("external findings have no safety boundary")
	}
}

func TestDiscoverArgumentValidation(t *testing.T) {
	for _, args := range [][]string{
		{"discover", "--timeout", "0s"}, {"discover", "--timeout=-1s"}, {"discover", "--timeout=no"},
		{"discover", "--only", ""}, {"discover", "--only=env,nope"}, {"discover", "--only=env,"},
		{"version", "--only=env"}, {"discover", "--context"}, {"discover", "extra"},
	} {
		var out bytes.Buffer
		if Run(args, &out, &out) == 0 {
			t.Fatalf("accepted %v", args)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatalf("invalid error JSON: %s", out.String())
		}
	}
}

func TestDiscoverCLITimeoutKeepsSuccessfulProbe(t *testing.T) {
	t.Setenv("SRE_SKIP_CLUSTER", "false")
	dir := t.TempDir()
	for name, body := range map[string]string{
		"aws": "#!/bin/sh\n/bin/sleep 2\n",
		"gh":  "#!/bin/sh\nprintf '%s\\n' '{\"nameWithOwner\":\"owner/repo\",\"url\":\"https://github.com/owner/repo\"}'\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	var out bytes.Buffer
	start := time.Now()
	code := Run([]string{"discover", "--only=aws,github", "--timeout=40ms"}, &out, &out)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("CLI did not enforce probe deadline: %s", out.String())
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Probes map[string]struct {
				Status   string `json:"status"`
				Findings struct {
					GitHub struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"github"`
				} `json:"findings"`
			} `json:"probes"`
		} `json:"data"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if code == 0 || envelope.OK || len(envelope.Errors) == 0 || envelope.Data.Probes["aws"].Status != "timeout" || envelope.Data.Probes["github"].Findings.GitHub.NameWithOwner != "owner/repo" {
		t.Fatalf("lost partial report: exit=%d %s", code, out.String())
	}
}

func TestDiscoverCLIContextPrecedence(t *testing.T) {
	for _, tc := range []struct{ name, env, explicit, want string }{
		{"explicit", "environment", "explicit", "explicit"}, {"environment", "environment", "", "environment"}, {"current", "", "", "current"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SRE_SKIP_CLUSTER", "false")
			t.Setenv("SRE_KUBE_CONTEXT", tc.env)
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			t.Setenv("DISCOVERY_CALL_LOG", log)
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$DISCOVERY_CALL_LOG"
if [ "$*" = 'config current-context' ]; then printf 'current\n'; exit 0; fi
case "$*" in
 *'version -o json') printf '%s\n' '{"clientVersion":{"gitVersion":"v1"},"serverVersion":{"gitVersion":"v1"}}';;
 *) printf '%s\n' '{"items":[]}';;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			args := []string{"discover", "--only=env"}
			if tc.explicit != "" {
				args = append(args, "--context", tc.explicit)
			}
			var out bytes.Buffer
			if code := Run(args, &out, &out); code != 0 {
				t.Fatalf("exit=%d %s", code, out.String())
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if line == "config current-context" {
					if tc.want != "current" {
						t.Fatal("explicit pin still resolved current context")
					}
					continue
				}
				if !strings.HasPrefix(line, "--context "+tc.want+" ") {
					t.Fatalf("unpinned call: %s", line)
				}
			}
			if !strings.Contains(out.String(), `"context":"`+tc.want+`"`) {
				t.Fatalf("reported wrong context: %s", out.String())
			}
		})
	}
}
