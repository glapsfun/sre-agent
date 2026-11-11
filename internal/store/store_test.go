package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func openStore(t *testing.T, root string) *Store {
	t.Helper()
	s, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func execSQL(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatal(err)
	}
}

func queryInt(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenPersistsPrivateDatabaseWithConnectionPragmas(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", "")
	s := openStore(t, root)
	execSQL(t, s.DB(), "CREATE TABLE sample (value TEXT NOT NULL)")
	execSQL(t, s.DB(), "INSERT INTO sample VALUES ('saved')")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(filepath.Join(root, ".sre-agent", "sre.db"+suffix))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Errorf("database file %q is not private: %o", suffix, info.Mode().Perm())
		}
	}
	info, err := os.Stat(filepath.Join(root, ".sre-agent"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory permissions: %v, %v", info, err)
	}
	// Force replacement connections to prove settings belong to the DSN.
	s.DB().SetMaxIdleConns(0)
	for range 2 {
		if got := queryInt(t, s.DB(), "PRAGMA busy_timeout"); got != 5000 {
			t.Errorf("busy timeout = %d, want 5000", got)
		}
		var mode string
		if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
			t.Fatalf("journal mode = %q, error %v", mode, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, root)
	if got := queryInt(t, s.DB(), "SELECT count(*) FROM sample WHERE value = 'saved'"); got != 1 {
		t.Fatalf("retained rows = %d", got)
	}
}

func TestOpenPreservesReservedPathCharacters(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo space # ? % & token=private")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, root)
	execSQL(t, s.DB(), "CREATE TABLE sample (value INTEGER)")
	if _, err := os.Stat(filepath.Join(root, ".sre-agent", "sre.db")); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRecreatesDeletedDatabaseEmpty(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	execSQL(t, s.DB(), "CREATE TABLE sample (value INTEGER)")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".sre-agent", "sre.db")); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, root)
	if got := queryInt(t, s.DB(), "SELECT count(*) FROM sqlite_master WHERE name = 'sample'"); got != 0 {
		t.Fatalf("old table survived recreation: %d", got)
	}
	if got := queryInt(t, s.DB(), "PRAGMA user_version"); got != 1 {
		t.Fatalf("new schema version = %d, want 1", got)
	}
}

func TestOpenUpgradesOlderDatabaseAndRetainsData(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".sre-agent"), 0700); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, filepath.Join(root, ".sre-agent", "sre.db"))
	execSQL(t, db, "CREATE TABLE sample (value INTEGER); INSERT INTO sample VALUES (42)")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, root)
	if got := queryInt(t, s.DB(), "PRAGMA user_version"); got != 1 {
		t.Fatalf("upgraded version = %d", got)
	}
	if got := queryInt(t, s.DB(), "SELECT value FROM sample"); got != 42 {
		t.Fatalf("retained value = %d", got)
	}
}

func TestOpenRefusesNewerDatabaseWithoutMutation(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".sre-agent"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".sre-agent", "sre.db")
	db := rawDB(t, path)
	execSQL(t, db, "CREATE TABLE future (value INTEGER); INSERT INTO future VALUES (42); PRAGMA user_version = 99")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), root)
	if s != nil || err == nil || !strings.Contains(err.Error(), "newer") || !strings.Contains(err.Error(), "99") {
		t.Fatalf("newer database: store %v, error %v", s, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("newer database changed: %v", err)
	}
}

func TestMigrationsUpgradeAndRollbackWholePendingBatch(t *testing.T) {
	db := rawDB(t, filepath.Join(t.TempDir(), "test.db"))
	v1 := Migration{Version: 1, SQL: "CREATE TABLE sample (value INTEGER); INSERT INTO sample VALUES (42)"}
	v2 := Migration{Version: 2, SQL: "ALTER TABLE sample ADD COLUMN note TEXT DEFAULT 'retained'"}
	if err := ApplyMigrations(t.Context(), db, []Migration{v1}); err != nil {
		t.Fatal(err)
	}
	bad := Migration{Version: 3, SQL: "CREATE TABLE partial (value INTEGER); INSERT INTO nonexistent VALUES (1)"}
	if err := ApplyMigrations(t.Context(), db, []Migration{v1, v2, bad}); err == nil {
		t.Fatal("invalid migration succeeded")
	}
	if got := queryInt(t, db, "PRAGMA user_version"); got != 1 {
		t.Errorf("failed batch changed version to %d", got)
	}
	if got := queryInt(t, db, "SELECT count(*) FROM pragma_table_info('sample')"); got != 1 {
		t.Errorf("failed batch left %d columns", got)
	}
	if got := queryInt(t, db, "SELECT count(*) FROM sqlite_master WHERE name = 'partial'"); got != 0 {
		t.Errorf("failed migration left partial table")
	}
	for range 2 {
		if err := ApplyMigrations(t.Context(), db, []Migration{v1, v2}); err != nil {
			t.Fatal(err)
		}
	}
	if got := queryInt(t, db, "SELECT value FROM sample WHERE note = 'retained'"); got != 42 {
		t.Errorf("migration lost data: %d", got)
	}
	if err := ApplyMigrations(t.Context(), db, []Migration{v1}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer migration version was accepted: %v", err)
	}
}

func TestMigrationsRejectInvalidSequencesBeforeWriting(t *testing.T) {
	for _, versions := range [][]int{{0}, {2}, {1, 1}, {1, 3}, {1, -1}} {
		t.Run(fmt.Sprint(versions), func(t *testing.T) {
			db := rawDB(t, filepath.Join(t.TempDir(), "test.db"))
			var migrations []Migration
			for _, version := range versions {
				migrations = append(migrations, Migration{version, "CREATE TABLE partial (value INTEGER)"})
			}
			if err := ApplyMigrations(t.Context(), db, migrations); err == nil {
				t.Fatal("invalid sequence accepted")
			}
			if got := queryInt(t, db, "SELECT count(*) FROM sqlite_master"); got != 0 {
				t.Fatalf("invalid sequence wrote schema: %d", got)
			}
		})
	}
}

func TestMigrationDeadlineRollsBackAndReleasesConnection(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := ApplyMigrations(ctx, s.DB(), []Migration{
		{1, ""},
		{2, `CREATE TABLE partial (value INTEGER);
			WITH RECURSIVE numbers(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM numbers WHERE n < 100000000)
			INSERT INTO partial SELECT sum(n) FROM numbers`},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("migration deadline = %v", err)
	}
	if got := queryInt(t, s.DB(), "PRAGMA user_version"); got != 1 {
		t.Fatalf("canceled migration changed version to %d", got)
	}
	if got := queryInt(t, s.DB(), "SELECT count(*) FROM sqlite_master WHERE name = 'partial'"); got != 0 {
		t.Fatal("canceled migration left partial table")
	}
	if err := ApplyMigrations(t.Context(), s.DB(), []Migration{{1, ""}, {2, "CREATE TABLE recovered (value INTEGER)"}}); err != nil {
		t.Fatalf("connection unusable after cancellation: %v", err)
	}
}

func TestCanceledContextsAndSafeErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	root := filepath.Join(t.TempDir(), "token=do-not-expose")
	if _, err := Open(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open cancellation = %v", err)
	}
	if _, err := ResolveRoot(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolveRoot cancellation = %v", err)
	}
	db := rawDB(t, filepath.Join(t.TempDir(), "test.db"))
	if err := ApplyMigrations(ctx, db, []Migration{{1, "CREATE TABLE sample (value INTEGER)"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("migration cancellation = %v", err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func() error{
		func() error { _, err := Open(t.Context(), root); return err },
		func() error { _, err := ResolveRoot(t.Context(), root); return err },
	} {
		if err := operation(); err == nil || strings.Contains(err.Error(), "do-not-expose") {
			t.Fatalf("unsafe or missing error: %v", err)
		}
	}
}

func git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func TestResolveRootFromNestedLinkedWorktree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "main")
	git(t, "init", root)
	git(t, "-C", root, "-c", "user.name=Store Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	linked := filepath.Join(t.TempDir(), "linked space # ? token=private")
	git(t, "-C", root, "worktree", "add", "--detach", linked)
	nested := filepath.Join(linked, "nested", "deep")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, nested} {
		got, err := ResolveRoot(t.Context(), dir)
		want := root
		if dir == nested {
			want = linked
		}
		want, _ = filepath.EvalSymlinks(want)
		if err != nil || got != want {
			t.Fatalf("root = %q, want %q, error %v", got, want, err)
		}
	}
	if _, err := ResolveRoot(t.Context(), t.TempDir()); err == nil {
		t.Fatal("non-repository accepted")
	}
}

type worker struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Reader
	errors bytes.Buffer
}

func startWorker(t *testing.T, mode, root, id string) *worker {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	w := &worker{cmd: exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStoreWorker$")}
	w.cmd.Env = append(os.Environ(), "SRE_STORE_WORKER="+mode, "SRE_STORE_ROOT="+root, "SRE_STORE_WORKER_ID="+id)
	w.cmd.Stderr = &w.errors
	output, err := w.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	w.output = bufio.NewReader(output)
	w.input, err = w.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.cmd.Process.Kill(); _ = w.cmd.Wait() })
	return w
}

func ready(t *testing.T, w *worker) {
	t.Helper()
	line, err := w.output.ReadString('\n')
	if err != nil || line != "READY\n" {
		t.Fatalf("worker readiness = %q, %v", line, err)
	}
}

func TestConcurrentProcessesOpenFreshDatabaseAndWrite(t *testing.T) {
	root := t.TempDir()
	workers := []*worker{startWorker(t, "write", root, "one"), startWorker(t, "write", root, "two")}
	for _, w := range workers {
		ready(t, w)
	}
	if _, err := os.Stat(filepath.Join(root, ".sre-agent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database exists before simultaneous first open: %v", err)
	}
	for _, w := range workers {
		if _, err := io.WriteString(w.input, "start\n"); err != nil {
			t.Fatal(err)
		}
		_ = w.input.Close()
	}
	for _, w := range workers {
		output, _ := io.ReadAll(w.output)
		if err := w.cmd.Wait(); err != nil {
			t.Fatalf("worker failed: %v\n%s\n%s", err, output, w.errors.String())
		}
	}
	s := openStore(t, root)
	if got := queryInt(t, s.DB(), "SELECT count(*) FROM writes"); got != 200 {
		t.Fatalf("rows = %d, want 200", got)
	}
	if got := queryInt(t, s.DB(), "SELECT count(DISTINCT id) FROM writes"); got != 200 {
		t.Fatalf("distinct rows = %d, want 200", got)
	}
}

func TestKilledMigrationRollsBack(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	w := startWorker(t, "migrate", root, "")
	ready(t, w)
	if err := w.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := w.cmd.Wait(); err == nil {
		t.Fatal("worker was not killed")
	}
	s = openStore(t, root)
	if got := queryInt(t, s.DB(), "PRAGMA user_version"); got != 1 {
		t.Fatalf("killed migration changed schema version: %d", got)
	}
	if got := queryInt(t, s.DB(), "SELECT count(*) FROM sqlite_master WHERE name = 'partial'"); got != 0 {
		t.Fatalf("killed migration left partial table")
	}
}

func TestStoreWorker(t *testing.T) {
	mode := os.Getenv("SRE_STORE_WORKER")
	if mode == "" {
		return
	}
	root := os.Getenv("SRE_STORE_ROOT")
	if mode == "write" {
		fmt.Println("READY")
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		// A non-idempotent fixture migration exposes duplicate application
		// when both processes observe the same old schema version.
		migrations = []Migration{{1, "CREATE TABLE writes (id TEXT PRIMARY KEY)"}}
		s := openStore(t, root)
		for i := range 100 {
			execSQL(t, s.DB(), "INSERT INTO writes VALUES (?)", fmt.Sprintf("%s-%d", os.Getenv("SRE_STORE_WORKER_ID"), i))
		}
		return
	}
	if err := sqlite.RegisterScalarFunction("wait_for_kill", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		fmt.Println("READY")
		_, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, root)
	if err := ApplyMigrations(t.Context(), s.DB(), []Migration{{1, ""}, {2, "CREATE TABLE partial (value INTEGER); SELECT wait_for_kill()"}}); err != nil {
		t.Fatal(err)
	}
}
