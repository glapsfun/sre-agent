package mcpserver

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPDraftReportOptionsAndMissingDatabase(t *testing.T) {
	repo := seededRepo(t, "customer_77 incident")
	cs := connectTest(t, New(Dependencies{}))
	input, _ := json.Marshal(map[string]any{"repo": repo, "run_id": "run", "detail": true, "stage": "post-incident", "rules": []map[string]string{{"name": "customer", "pattern": "customer_77", "action": "redact"}}})
	result := callTest(t, cs, "report", string(input))
	raw := resultJSON(t, result)
	if result.IsError || !strings.Contains(string(raw), `"draft":true`) || !strings.Contains(string(raw), `"detail":`) || strings.Contains(string(raw), "customer_77") {
		t.Fatalf("draft report options failed: %s", raw)
	}
	missing := t.TempDir()
	if raw, err := exec.Command("git", "init", "-q", missing).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, raw)
	}
	input, _ = json.Marshal(map[string]string{"repo": missing, "run_id": "run"})
	if result := callTest(t, cs, "report", string(input)); !result.IsError {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(filepath.Join(missing, ".sre-agent")); !os.IsNotExist(err) {
		t.Fatal("MCP report initialized database")
	}
}
