package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/glapsfun/sre-agent/internal/logs"
)

func TestLogsFoldCLIJSONAndText(t *testing.T) {
	var outputs []string
	for i := 0; i < 2; i++ {
		var out, stderr bytes.Buffer
		code := RunWithInput([]string{"logs", "fold", "--max-patterns=1"}, strings.NewReader("INFO ok\nERROR password=hidden\n"), &out, &stderr)
		var envelope struct {
			OK     bool        `json:"ok"`
			Data   logs.Report `json:"data"`
			Gaps   []string    `json:"gaps"`
			Errors []string    `json:"errors"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if code != 0 || !envelope.OK || stderr.Len() != 0 || envelope.Data.TotalLines != 2 || envelope.Data.OmittedLines != 1 || len(envelope.Data.Patterns) != 1 || envelope.Data.Patterns[0].Level != "error" || envelope.Gaps == nil || envelope.Errors == nil {
			t.Fatalf("code=%d output=%s", code, out.String())
		}
		if strings.Contains(out.String(), "hidden") || !strings.Contains(envelope.Data.Patterns[0].Sample, "BEGIN EXTERNAL DATA") {
			t.Fatal("unsafe sample")
		}
		outputs = append(outputs, out.String())
	}
	marker := regexp.MustCompile(`[a-f0-9]{32}`)
	if outputs[0] == outputs[1] || marker.ReplaceAllString(outputs[0], "MARKER") != marker.ReplaceAllString(outputs[1], "MARKER") {
		t.Fatal("incorrect marker freshness or deterministic data")
	}
	var out bytes.Buffer
	if code := RunWithInput([]string{"--format=text", "logs", "fold"}, strings.NewReader("WARN retry\n"), &out, &out); code != 0 || !strings.Contains(out.String(), "Total lines: 1") || !strings.Contains(out.String(), "warn") || !strings.Contains(out.String(), "BEGIN EXTERNAL DATA") {
		t.Fatalf("text: %d %s", code, out.String())
	}
}

func TestLogsFoldCLIValidation(t *testing.T) {
	for _, args := range [][]string{
		{"logs"}, {"logs", "tail"}, {"logs", "fold", "extra"}, {"logs", "fold", "--max-patterns=0"},
		{"logs", "fold", "--max-patterns=-1"}, {"logs", "fold", "--max-patterns=no"}, {"logs", "fold", "--max-patterns"},
		{"logs", "fold", "--pod=app"}, {"logs", "fold", "--namespace=ns"}, {"logs", "fold", "--previous"},
		{"logs", "fold", "--context=ctx"}, {"logs", "fold", "--since=0s"}, {"logs", "fold", "--since=bad"},
		{"logs", "fold", "--previous=bad"}, {"logs", "fold", "--only=env"}, {"logs", "fold", "--timeout=1s"},
		{"discover", "--pod=app"}, {"version", "--max-patterns=2"}, {"logs", "fold", "--follow"},
	} {
		var out bytes.Buffer
		if code := RunWithInput(args, strings.NewReader(""), &out, &out); code == 0 || !json.Valid(out.Bytes()) {
			t.Fatalf("args=%v code=%d output=%s", args, code, out.String())
		}
	}
}

func TestLogsFoldCLIPod(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = --context ] && [ \"$2\" = explicit ] || exit 9\nprintf 'ERROR failed\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("SRE_KUBE_CONTEXT", "environment")
	var out bytes.Buffer
	if code := RunWithInput([]string{"logs", "fold", "--pod=app", "--namespace=ns", "--context=explicit", "--container=app", "--previous", "--since=1h"}, strings.NewReader("INFO stdin must not be read"), &out, &out); code != 0 || !strings.Contains(out.String(), `"total_lines":1`) || strings.Contains(out.String(), "stdin must") {
		t.Fatalf("%d %s", code, out.String())
	}
}
