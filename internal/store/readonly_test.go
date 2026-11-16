package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyDoesNotInitializeOrMigrate(t *testing.T) {
	root := t.TempDir()
	if db, err := OpenReadOnly(t.Context(), root); err == nil {
		db.Close()
		t.Fatal("missing database was opened")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("read initialized storage: %v %v", entries, err)
	}
	for _, version := range []int{0, 1, len(migrations) - 1, len(migrations) + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, ".sre-agent")
			if err := os.Mkdir(dir, 0750); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "sre.db")
			raw := rawDB(t, path)
			execSQL(t, raw, fmt.Sprintf("PRAGMA user_version=%d", version))
			raw.Close()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if db, err := OpenReadOnly(t.Context(), root); err == nil {
				db.Close()
				t.Fatal("incompatible database accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("reader modified schema")
			}
		})
	}
}

func TestReadOnlyPreservesDatabaseAndRejectsWrites(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo #? space")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	writable := openStore(t, root)
	execSQL(t, writable.DB(), "CREATE TABLE sample(value TEXT)")
	execSQL(t, writable.DB(), "INSERT INTO sample VALUES ('recorded')")
	writable.Close()
	dir := filepath.Join(root, ".sre-agent")
	path := filepath.Join(dir, "sre.db")
	if err := os.Chmod(dir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenReadOnly(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.DB().SetMaxIdleConns(0)
	for range 2 {
		if queryInt(t, db.DB(), "SELECT count(*) FROM sample WHERE value='recorded'") != 1 {
			t.Fatal("snapshot unreadable")
		}
		if _, err := db.DB().Exec("DELETE FROM sample"); err == nil {
			t.Fatal("read-only connection wrote data")
		}
	}
	db.Close()
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("database changed")
	}
	for p, want := range map[string]os.FileMode{dir: 0750, path: 0640} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("permissions changed: %s %v %v", p, info, err)
		}
	}
}

func TestReadOnlyRejectsStoreSymlinks(t *testing.T) {
	for _, directoryLink := range []bool{false, true} {
		root, target := t.TempDir(), t.TempDir()
		db := openStore(t, target)
		db.Close()
		dir := filepath.Join(root, ".sre-agent")
		if directoryLink {
			if err := os.Symlink(filepath.Join(target, ".sre-agent"), dir); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(target, ".sre-agent", "sre.db"), filepath.Join(dir, "sre.db")); err != nil {
				t.Fatal(err)
			}
		}
		if reader, err := OpenReadOnly(t.Context(), root); err == nil {
			reader.Close()
			t.Fatal("read followed store symlink")
		}
	}
}
