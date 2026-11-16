package report

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestExportDraftBundleAndRepeatProtection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review [a](draft)#%.md")
	doc := snapshotFor(t, runFrom(t, validatedEntries(t)), SnapshotOptions{Detail: true})
	receipt, err := Export(path, doc)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := os.ReadFile(receipt.Path)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := os.ReadFile(receipt.DetailPath)
	if err != nil || string(detail) != doc.Detail {
		t.Fatalf("detail differs: %v", err)
	}
	index, err := os.ReadFile(receipt.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{string(summary), string(index)} {
		start := strings.LastIndex(content, "](")
		if start < 0 {
			t.Fatalf("relative link absent: %s", content)
		}
		end := strings.Index(content[start+2:], ")")
		if end < 0 {
			t.Fatal("broken markdown link")
		}
		link, err := url.PathUnescape(content[start+2 : start+2+end])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, link)); err != nil {
			t.Fatalf("relative link broken: %s: %v", link, err)
		}
	}
	if !strings.Contains(string(index), "Draft") || !strings.Contains(string(index), "run") || !strings.Contains(string(index), "10") {
		t.Fatal("index lost source identity")
	}
	for _, p := range []string{receipt.Path, receipt.DetailPath, receipt.IndexPath} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe permissions: %v %v", info, err)
		}
	}
	if _, err := Export(path, doc); !errors.Is(err, os.ErrExist) {
		t.Fatalf("repeat must reject existing output: %v", err)
	}
	for p, before := range map[string]string{path: string(summary), receipt.DetailPath: string(detail), receipt.IndexPath: string(index)} {
		after, err := os.ReadFile(p)
		if err != nil || string(after) != before {
			t.Fatal("repeat changed existing files")
		}
	}
	if _, err := Export(filepath.Join(dir, "second.md"), doc); err != nil {
		t.Fatal(err)
	}
	updated, _ := os.ReadFile(receipt.IndexPath)
	if !strings.Contains(string(updated), "second.md") || !strings.Contains(string(updated), "review") {
		t.Fatal("index lost earlier export")
	}
}

func TestExportRejectsForeignIndexAndOccupiedPaths(t *testing.T) {
	doc := snapshotFor(t, runFrom(t, nil), SnapshotOptions{Detail: true})
	for _, conflict := range []string{"summary", "detail", "index", "index-directory", "summary-symlink", "index-symlink"} {
		t.Run(conflict, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "report.md")
			target := path
			if conflict == "detail" {
				target = filepath.Join(dir, "report.details.md")
			}
			if strings.HasPrefix(conflict, "index") {
				target = filepath.Join(dir, "index.md")
			}
			if strings.HasSuffix(conflict, "symlink") {
				if err := os.Symlink("absent-target", target); err != nil {
					t.Fatal(err)
				}
			} else if conflict == "index-directory" {
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(target, []byte("existing content"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Export(path, doc); err == nil {
				t.Fatal("existing path overwritten")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("failure left partial outputs: %v %v", entries, err)
			}
		})
	}
}

func TestExportSerializesConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	doc := snapshotFor(t, runFrom(t, nil), SnapshotOptions{})
	var group sync.WaitGroup
	errors := make(chan error, 2)
	for _, name := range []string{"one.md", "two.md"} {
		group.Go(func() { _, err := Export(filepath.Join(dir, name), doc); errors <- err })
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil && !strings.Contains(err.Error(), "busy") {
			t.Fatal(err)
		}
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.md", "two.md"} {
		_, err := os.Stat(filepath.Join(dir, name))
		if err == nil && !strings.Contains(string(index), name) {
			t.Fatal("successful export absent from index")
		}
	}
}

func TestExportRejectsIndexNameAndMissingParent(t *testing.T) {
	doc := snapshotFor(t, runFrom(t, nil), SnapshotOptions{})
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "index.md"), filepath.Join(dir, "INDEX.md"), filepath.Join(dir, "missing", "report.md")} {
		if _, err := Export(path, doc); err == nil {
			t.Fatalf("invalid path accepted: %s", path)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("invalid export created files")
	}
}

func TestExportFilenameLinksCannotBecomeAbsoluteURIs(t *testing.T) {
	dir := t.TempDir()
	doc := snapshotFor(t, runFrom(t, nil), SnapshotOptions{Detail: true})
	receipt, err := Export(filepath.Join(dir, "javascript:alert(1).md"), doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{receipt.Path, receipt.IndexPath} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		start := strings.LastIndex(string(raw), "](")
		if start < 0 {
			t.Fatal("output link absent")
		}
		target := strings.SplitN(string(raw)[start+2:], ")", 2)[0]
		u, err := url.Parse(target)
		if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" {
			t.Fatalf("export link is not relative: %q %v", target, err)
		}
		if _, err := os.Stat(filepath.Join(dir, u.Path)); err != nil {
			t.Fatalf("export link does not reach file: %q: %v", target, err)
		}
	}
}
