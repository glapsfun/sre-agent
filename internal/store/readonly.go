package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// OpenReadOnly reads existing storage without initialization, migrations or
// permission changes. SQLite may use WAL sidecars to coordinate a live snapshot.
func OpenReadOnly(ctx context.Context, root string) (*Store, error) {
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
	directory := filepath.Join(root, ".sre-agent")
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, storeError("existing investigation store unavailable", err)
	}
	if !info.IsDir() {
		return nil, errors.New("store directory must be a directory, not a symlink")
	}
	path := filepath.Join(directory, "sre.db")
	info, err = os.Lstat(path)
	if err != nil {
		return nil, storeError("existing investigation database unavailable", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("store database must be a regular file, not a symlink or special file")
	}
	u := url.URL{Scheme: "file", Path: path}
	query := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)", "query_only(1)"}}
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, storeError("open read-only store", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, storeError("read schema version", err)
	}
	if version != len(migrations) {
		db.Close()
		return nil, fmt.Errorf("report requires schema version %d; database has %d (no migration performed)", len(migrations), version)
	}
	return &Store{db: db}, nil
}
