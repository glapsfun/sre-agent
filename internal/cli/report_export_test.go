package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportDraftExportCLI(t *testing.T) {
	root := ledgerRepo(t)
	seedIncidentCLI(t, root, "selected", "restore image")
	policy := filepath.Join(root, "rules.json")
	if err := os.WriteFile(policy, []byte(`{"rules":[{"name":"image","pattern":"restore image","action":"redact"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "draft.md")
	args := []string{"report", "selected", "--repo", root, "--stage", "post-incident", "--detail", "--rules", policy, "--out", path}
	var out, stderr bytes.Buffer
	if code := RunWithInput(args, nil, &out, &stderr); code != 0 {
		t.Fatalf("export: %d %s %s", code, &out, &stderr)
	}
	var env Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	data := env.Data.(map[string]any)
	if data["draft"] != true || data["stage"] != "post-incident" || data["generated_at"] == nil || data["detail_path"] != filepath.Join(root, "draft.details.md") || data["index_path"] != filepath.Join(root, "index.md") {
		t.Fatal(data)
	}
	for _, name := range []string{"draft.md", "draft.details.md", "index.md"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !bytes.Contains(raw, []byte("Draft")) || bytes.Contains(raw, []byte("restore image")) {
			t.Fatalf("unsafe/missing draft %s: %v", name, err)
		}
	}
	out.Reset()
	if code := RunWithInput(args, nil, &out, &stderr); code != 1 || !strings.Contains(out.String(), "already exists") {
		t.Fatalf("repeat overwrote output: %d %s", code, &out)
	}
}

func TestReportMissingDatabaseDoesNotInitialize(t *testing.T) {
	root := ledgerRepo(t)
	code, env, _ := incidentCLI(t, root, "report", "missing")
	if code != 1 || env.OK {
		t.Fatal("missing database succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, ".sre-agent")); !os.IsNotExist(err) {
		t.Fatal("report initialized database")
	}
}

func TestDraftReportFlagsAreIsolated(t *testing.T) {
	root := ledgerRepo(t)
	for _, args := range [][]string{
		{"version", "--detail"}, {"run", "show", "one", "--stage", "interim"},
		{"report", "one", "--detail=maybe"}, {"report", "one", "--stage=resolved"},
		{"report", "one", "--rules="},
	} {
		code, env, _ := incidentCLI(t, root, args...)
		if code != 1 || env.OK {
			t.Fatalf("invalid flags accepted: %v", args)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".sre-agent")); !os.IsNotExist(err) {
		t.Fatal("invalid options initialized storage")
	}
}
