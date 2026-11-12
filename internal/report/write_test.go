package report

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReportOutMatchesDocument(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "report.md")
	const old = "previous report\n"
	if err := os.WriteFile(path, []byte(old), 0644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "old-copy")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	document := Document{RunID: "one", Status: "unresolved", Markdown: "# Recorded problem\n\nnot recorded\n"}
	for range 2 {
		if err := WriteFile(path, document); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != document.Markdown {
			t.Fatalf("artifact: %q %v", data, err)
		}
		data, err = os.ReadFile(alias)
		if err != nil || string(data) != old {
			t.Fatal("old inode was modified instead of atomic replacement")
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("permissions: %v %v", info, err)
		}
		files, err := os.ReadDir(directory)
		if err != nil || len(files) != 2 {
			t.Fatalf("temporary files remain: %v %v", files, err)
		}
	}
}

func TestReportWriteFailurePreservesExistingFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "report.md")
	const old = "previous complete report\n"
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(directory, "existing-directory")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{path + "/child", blocked} {
		if err := WriteFile(target, Document{Markdown: "replacement"}); err == nil {
			t.Fatalf("write to %q succeeded", target)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != old {
			t.Fatalf("existing file lost: %q %v", data, err)
		}
		files, err := os.ReadDir(directory)
		if err != nil || len(files) != 2 {
			t.Fatalf("temporary files remain: %v %v", files, err)
		}
	}
}
