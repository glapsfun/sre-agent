package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/safety"
	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
)

type Store struct{ db *sql.DB }

// Open uses an explicit repository root without requiring git. An empty root
// discovers the repository containing the current working directory.
func Open(ctx context.Context, root string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	if root == "" {
		root, err = ResolveRoot(ctx, ".")
	} else {
		root, err = filepath.Abs(root)
	}
	if err != nil {
		return nil, storeError("resolve repository root", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, storeError("read repository root", err)
	}
	if !info.IsDir() {
		return nil, errors.New("repository root is not a directory")
	}
	directory := filepath.Join(root, ".sre-agent")
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, storeError("create store directory", err)
	}
	// Only the repository root may resolve through symlinks; store-owned
	// entries must not redirect permission changes or database writes.
	info, err = os.Lstat(directory)
	if err != nil {
		return nil, storeError("inspect store directory", err)
	}
	if !info.IsDir() {
		return nil, errors.New("store directory must be a directory, not a symlink")
	}
	path := filepath.Join(directory, "sre.db")
	info, err = os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, storeError("inspect store file", err)
	}
	if err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("store database must be a regular file, not a symlink or special file")
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, storeError("protect store directory", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, storeError("create store file", err)
	}
	err = file.Chmod(0600)
	closeErr := file.Close()
	if err != nil {
		return nil, storeError("protect store file", err)
	}
	if closeErr != nil {
		return nil, storeError("close store file", closeErr)
	}
	u := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	// The driver applies this to every connection, including replacements.
	query.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, storeError("open store", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := ApplyMigrations(ctx, db, migrations); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Journal mode persists in the file. Check the schema first so an older
	// binary cannot change the journal mode of a newer database.
	if err := enableWAL(ctx, db); err != nil {
		_ = db.Close()
		return nil, storeError("enable store WAL", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the shared connection pool. Feature write APIs must redact secret
// values with internal/safety before storing content.
func (s *Store) DB() *sql.DB { return s.db }

func enableWAL(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		var mode string
		err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode)
		if err == nil {
			if mode != "wal" {
				return errors.New("database did not enter WAL mode")
			}
			return nil
		}
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
			return err
		}
		// Simultaneous journal-mode changes may bypass SQLite's busy handler.
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// ResolveRoot supports linked worktrees, whose .git entry is a file.
func ResolveRoot(ctx context.Context, directory string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if directory == "" {
		directory = "."
	}
	// Paths must retain their exact bytes; the shared runner redacts stdout.
	cmd := exec.CommandContext(ctx, "git", "-C", directory, "rev-parse", "--show-toplevel")
	cmd.WaitDelay = 100 * time.Millisecond
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", errors.New("cannot resolve repository root with git")
	}
	root := strings.TrimSuffix(string(output), "\n")
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("git returned an invalid repository root")
	}
	return root, nil
}

func storeError(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return fmt.Errorf("%s: %s", operation, safety.NewRedactor().Redact(err.Error()))
}
