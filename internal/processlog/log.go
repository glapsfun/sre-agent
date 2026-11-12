// Package processlog records sanitized metadata for observed investigator calls.
package processlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/guard"
	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/store"
)

var ErrConflict = errors.New("process call conflicts with its recorded identity or events")

type Entry struct {
	RecordedAt    time.Time `json:"recorded_at"`
	RunID         string    `json:"run_id"`
	SessionID     string    `json:"session_id"`
	ToolUseID     string    `json:"tool_use_id"`
	Agent         string    `json:"agent"`
	Tool          string    `json:"tool"`
	Command       string    `json:"command"`
	Path          string    `json:"path"`
	Event         string    `json:"-"`
	GuardDecision *string   `json:"guard_decision"`
	ExitStatus    *int64    `json:"exit_status"`
	DurationMS    *int64    `json:"duration_ms"`
}

type Call struct {
	Entry
	Outcome string `json:"outcome"`
}

// Shell flags with a separate argument need protection in addition to the shared
// redactor's assignments, JSON keys, authorization headers and URL credentials.
var secretFlag = regexp.MustCompile(`(?i)((?:--[a-z0-9_-]*(?:password|passwd|token|secret|api[_-]?key|access[_-]?key|credential|authorization)[a-z0-9_-]*|--user|-u)(?:\s+|=))("(?:\\.|[^"\\])*"|'[^'\n]*'|[^\s;|&]+)`)

var attachedUser = regexp.MustCompile(`(^|\s)(-u)[^\s;|&]+`)

func sanitize(ctx context.Context, value string) (string, error) {
	value = attachedUser.ReplaceAllString(value, "${1}${2}[REDACTED]")
	value = secretFlag.ReplaceAllString(value, "${1}[REDACTED]")
	return safety.NewRedactor().RedactContext(ctx, value)
}

func prepare(ctx context.Context, e Entry) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	if e.RecordedAt.IsZero() || e.RecordedAt.Year() < 0 || e.RecordedAt.Year() > 9999 || !guard.IsInvestigator(e.Agent) {
		return Entry{}, errors.New("invalid process observation")
	}
	for _, value := range []string{e.RunID, e.SessionID, e.ToolUseID, e.Agent, e.Tool} {
		if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return Entry{}, errors.New("invalid process identity")
		}
		clean, err := sanitize(ctx, value)
		if err != nil {
			return Entry{}, err
		}
		if clean != value {
			return Entry{}, errors.New("unsafe process identity")
		}
	}
	if e.DurationMS != nil && *e.DurationMS < 0 {
		return Entry{}, errors.New("duration must be nonnegative")
	}
	switch e.Event {
	case "pre":
		if e.GuardDecision == nil || (*e.GuardDecision != "allowed" && *e.GuardDecision != "blocked") || e.ExitStatus != nil || e.DurationMS != nil {
			return Entry{}, errors.New("invalid pre observation")
		}
	case "success", "failure":
		if e.GuardDecision != nil {
			return Entry{}, errors.New("post observation cannot supply a guard decision")
		}
	default:
		return Entry{}, errors.New("invalid process event")
	}
	var err error
	e.Command, err = sanitize(ctx, e.Command)
	if err != nil {
		return Entry{}, err
	}
	e.Path, err = sanitize(ctx, e.Path)
	if err != nil {
		return Entry{}, err
	}
	e.RecordedAt = e.RecordedAt.UTC()
	return e, nil
}

// Record pins identity and the validated run on first observation. A write first
// acquires SQLite's writer lock so separate hook processes reconcile atomically.
func Record(ctx context.Context, s *store.Store, e Entry) error {
	e, err := prepare(ctx, e)
	if err != nil {
		return err
	}
	if _, err := ledger.New(s, nil).Load(ctx, e.RunID); err != nil {
		return err
	}
	conn, err := s.DB().Conn(ctx)
	if err != nil {
		return storageError(ctx, err)
	}
	defer conn.Close()
	for _, pragma := range []struct {
		name  string
		value int
	}{{"foreign_keys", 1}, {"synchronous", 2}} {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA %s=%d", pragma.name, pragma.value)); err != nil {
			return storageError(ctx, err)
		}
		var value int
		if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma.name).Scan(&value); err != nil {
			return storageError(ctx, err)
		}
		if value != pragma.value {
			return errors.New("process log durability setting was not applied")
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return storageError(ctx, err)
	}
	defer tx.Rollback()
	outcome := "unknown"
	if e.Event == "success" {
		outcome = "succeeded"
	}
	if e.Event == "failure" {
		outcome = "failed"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO commands(session_id,tool_use_id,run_id,recorded_at,agent,tool,command,path,guard_decision,outcome,exit_status,duration_ms)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(session_id,tool_use_id) DO NOTHING`, e.SessionID, e.ToolUseID, e.RunID, e.RecordedAt.Format("2006-01-02T15:04:05.000000000Z"), e.Agent, e.Tool, e.Command, e.Path, e.GuardDecision, outcome, e.ExitStatus, e.DurationMS)
	if err != nil {
		return storageError(ctx, err)
	}
	c, err := scanCall(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM commands WHERE session_id=? AND tool_use_id=?", e.SessionID, e.ToolUseID))
	if err != nil {
		return storageError(ctx, err)
	}
	if c.RunID != e.RunID {
		return ErrConflict
	}
	conflict := c.Outcome == "conflict"
	if c.Agent != e.Agent || c.Tool != e.Tool || c.Command != e.Command || c.Path != e.Path {
		conflict = true
	} else {
		if e.GuardDecision != nil {
			if c.GuardDecision != nil && *c.GuardDecision != *e.GuardDecision {
				conflict = true
			} else if c.GuardDecision == nil {
				c.GuardDecision = e.GuardDecision
			}
		}
		if outcome != "unknown" {
			if c.Outcome != "unknown" && c.Outcome != outcome {
				conflict = true
			} else {
				c.Outcome = outcome
			}
			if mergeMetric(&c.ExitStatus, e.ExitStatus) || mergeMetric(&c.DurationMS, e.DurationMS) {
				conflict = true
			}
		}
	}
	if c.GuardDecision != nil && *c.GuardDecision == "blocked" && c.Outcome != "unknown" {
		conflict = true
	}
	if conflict {
		c.Outcome = "conflict"
		c.ExitStatus = nil
		c.DurationMS = nil
	}
	_, err = tx.ExecContext(ctx, "UPDATE commands SET guard_decision=?,outcome=?,exit_status=?,duration_ms=? WHERE session_id=? AND tool_use_id=?", c.GuardDecision, c.Outcome, c.ExitStatus, c.DurationMS, e.SessionID, e.ToolUseID)
	if err != nil {
		return storageError(ctx, err)
	}
	if err := tx.Commit(); err != nil {
		return storageError(ctx, err)
	}
	if conflict {
		return ErrConflict
	}
	return nil
}

func mergeMetric(stored **int64, incoming *int64) bool {
	if incoming == nil {
		return false
	}
	if *stored != nil {
		return **stored != *incoming
	}
	*stored = incoming
	return false
}

const columns = "recorded_at,run_id,session_id,tool_use_id,agent,tool,command,path,guard_decision,outcome,exit_status,duration_ms"

func scanCall(row interface{ Scan(...any) error }) (Call, error) {
	var c Call
	var recorded string
	if err := row.Scan(&recorded, &c.RunID, &c.SessionID, &c.ToolUseID, &c.Agent, &c.Tool, &c.Command, &c.Path, &c.GuardDecision, &c.Outcome, &c.ExitStatus, &c.DurationMS); err != nil {
		return Call{}, err
	}
	var err error
	c.RecordedAt, err = time.Parse(time.RFC3339Nano, recorded)
	return c, err
}

func storageError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("process call not found")
	}
	// Driver errors may include externally supplied text or filesystem paths.
	return errors.New("process log storage operation failed")
}
