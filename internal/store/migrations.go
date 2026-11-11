package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

type Migration struct {
	Version int
	SQL     string
}

// Append immutable, contiguous migrations here as features add their tables.
// Version 1 establishes schema metadata without creating feature-owned tables.
var migrations = []Migration{{Version: 1}}

// ApplyMigrations applies all pending versions atomically. SQL is trusted,
// compiled-in schema SQL and must not contain transaction control statements.
// Version metadata uses SQLite's transactional user_version header field.
func ApplyMigrations(ctx context.Context, db *sql.DB, migrations []Migration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, migration := range migrations {
		if migration.Version != i+1 {
			return errors.New("migration versions must be contiguous starting at 1")
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return storeError("acquire migration connection", err)
	}
	defer conn.Close()
	committed := false
	// BEGIN can acquire the lock even when the driver reports cancellation.
	// Install cleanup first so that connection cannot retain a transaction.
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
			// A connection with an unclosed manual transaction must not return
			// to the pool, even when cancellation prevents rollback.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	// Lock before reading the version so competing processes cannot both
	// decide the same migrations are pending and then race to upgrade.
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return storeError("lock schema migration", err)
	}
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return storeError("read schema version", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, len(migrations))
	}
	if version < 0 {
		return errors.New("database schema version is invalid")
	}
	for _, migration := range migrations[version:] {
		if _, err := conn.ExecContext(ctx, migration.SQL); err != nil {
			return storeError(fmt.Sprintf("apply schema migration %d", migration.Version), err)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", migration.Version)); err != nil {
			return storeError("write schema version", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return storeError("commit schema migrations", err)
	}
	committed = true
	return nil
}
