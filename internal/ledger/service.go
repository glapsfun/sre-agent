package ledger

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/store"
)

var (
	ErrNotFound         = errors.New("ledger record not found")
	ErrConflict         = errors.New("ledger identifier already has different contents")
	ErrInvalidReference = errors.New("ledger reference must name an earlier entry of the required kind in this run")
	ErrCorrupt          = errors.New("stored ledger is invalid")
	errReplay           = errors.New("idempotent ledger replay")
)

type StartRequest struct{ ID, Problem, SessionID string }
type AddRequest struct {
	RunID, ID string
	Kind      Kind
	Payload   json.RawMessage
}

type Querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Service struct {
	db  *sql.DB
	now func() time.Time
	// Tests pause after all writes but before commit to prove crash rollback.
	beforeCommit func(*sql.Conn) error
}

func New(s *store.Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: s.DB(), now: now}
}

func newID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("cannot generate ledger identifier")
	}
	return prefix + hex.EncodeToString(value[:]), nil
}

func (s *Service) timestamp() (time.Time, error) {
	at := s.now().UTC()
	if at.IsZero() || at.Year() < 0 || at.Year() > 9999 {
		return time.Time{}, errors.New("invalid ledger clock time")
	}
	return at, nil
}

func (s *Service) Start(ctx context.Context, req StartRequest) (Run, error) {
	if err := ctx.Err(); err != nil {
		return Run{}, err
	}
	if !utf8.ValidString(req.Problem) || strings.TrimSpace(req.Problem) == "" {
		return Run{}, errors.New("run problem must be nonempty UTF-8 text")
	}
	if req.SessionID != "" && !validSessionID(req.SessionID) {
		return Run{}, errors.New("invalid session identifier")
	}
	var err error
	if req.ID == "" {
		req.ID, err = newID("run-")
	}
	if err != nil {
		return Run{}, err
	}
	if !validID(req.ID) {
		return Run{}, errors.New("invalid run identifier")
	}
	problem, err := safety.NewRedactor().RedactContext(ctx, req.Problem)
	if err != nil {
		return Run{}, err
	}
	at, err := s.timestamp()
	if err != nil {
		return Run{}, err
	}
	var initial *preparedEntry
	if req.SessionID != "" {
		id, err := newID("entry-")
		if err != nil {
			return Run{}, err
		}
		raw, err := json.Marshal(MetadataUpdate{SessionID: &req.SessionID})
		if err != nil {
			return Run{}, errors.New("cannot encode initial session metadata")
		}
		prepared, err := prepareEntry(ctx, AddRequest{RunID: req.ID, ID: id, Kind: KindMetadata, Payload: raw}, at)
		if err != nil {
			return Run{}, err
		}
		initial = &prepared
	}
	var run Run
	err = s.write(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, "INSERT INTO runs(id,problem,created_at) VALUES (?,?,?) ON CONFLICT(id) DO NOTHING", req.ID, problem, at.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted == 0 {
			var original string
			if err := conn.QueryRowContext(ctx, "SELECT problem FROM runs WHERE id=?", req.ID).Scan(&original); err != nil {
				return err
			}
			if original != problem {
				return ErrConflict
			}
			run, err = LoadRun(ctx, conn, req.ID)
			if err != nil {
				return err
			}
			// A start retry must not restore a session that was explicitly rebound.
			return errReplay
		}
		if initial != nil {
			if _, err := appendPrepared(ctx, conn, *initial); err != nil {
				return err
			}
		}
		run, err = LoadRun(ctx, conn, req.ID)
		return err
	})
	if err != nil {
		return Run{}, ledgerError(ctx, "start run", err)
	}
	return run, nil
}

func (s *Service) Add(ctx context.Context, req AddRequest) (Entry, error) {
	if !validID(req.RunID) || !validID(req.ID) {
		return Entry{}, errors.New("invalid run or entry identifier")
	}
	at, err := s.timestamp()
	if err != nil {
		return Entry{}, err
	}
	prepared, err := prepareEntry(ctx, req, at)
	if err != nil {
		return Entry{}, err
	}
	var entry Entry
	err = s.write(ctx, func(conn *sql.Conn) error {
		var err error
		entry, err = appendPrepared(ctx, conn, prepared)
		return err
	})
	if err != nil {
		return Entry{}, ledgerError(ctx, "add entry", err)
	}
	return entry, nil
}

type preparedEntry struct {
	entry   Entry
	payload any
}

func prepareEntry(ctx context.Context, req AddRequest, at time.Time) (preparedEntry, error) {
	if err := ctx.Err(); err != nil {
		return preparedEntry{}, err
	}
	payload, err := DecodePayload(req.Kind, req.Payload)
	if err != nil {
		return preparedEntry{}, err
	}
	value := reflect.New(reflect.TypeOf(payload))
	value.Elem().Set(reflect.ValueOf(payload))
	if err := redactPayload(ctx, value.Elem()); err != nil {
		return preparedEntry{}, err
	}
	canonical, err := json.Marshal(value.Elem().Interface())
	if err != nil {
		return preparedEntry{}, errors.New("cannot encode ledger payload")
	}
	payload, err = DecodePayload(req.Kind, canonical)
	if err != nil {
		return preparedEntry{}, fmt.Errorf("redacted ledger payload: %w", err)
	}
	return preparedEntry{entry: Entry{ID: req.ID, RunID: req.RunID, Kind: req.Kind, RecordedAt: at, Payload: canonical}, payload: payload}, nil
}

// Typed identity fields retain their exact meaning; only human text is redacted.
func identityField(owner reflect.Type, name string) bool {
	switch owner {
	case reflect.TypeFor[MetadataUpdate]():
		return name == "SessionID"
	case reflect.TypeFor[Hypothesis]():
		return oneOf(name, "Supporting", "Contradicting", "Supersedes")
	case reflect.TypeFor[Criterion]():
		return name == "ID"
	case reflect.TypeFor[Option]():
		return name == "HypothesisIDs"
	case reflect.TypeFor[Approval]():
		return name == "OptionID"
	case reflect.TypeFor[Action]():
		return oneOf(name, "OptionID", "ApprovalID", "EvidenceIDs")
	case reflect.TypeFor[Validation]():
		return oneOf(name, "ExpectedEntryID", "CriterionID", "ActionID", "EvidenceIDs")
	case reflect.TypeFor[SafetyViolation]():
		return name == "Acknowledges"
	}
	return false
}

func redactPayload(ctx context.Context, value reflect.Value) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if value.Type() == reflect.TypeFor[time.Time]() {
		return nil
	}
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			return redactPayload(ctx, value.Elem())
		}
	case reflect.Struct:
		for i := range value.NumField() {
			if identityField(value.Type(), value.Type().Field(i).Name) {
				continue
			}
			if err := redactPayload(ctx, value.Field(i)); err != nil {
				return err
			}
		}
	case reflect.Slice:
		for i := range value.Len() {
			if err := redactPayload(ctx, value.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		// Dynamic cloud-coordinate keys need key-aware redaction and collision handling.
		raw, err := json.Marshal(value.Interface())
		if err != nil {
			return errors.New("cannot encode ledger coordinates")
		}
		redacted, err := safety.NewRedactor().RedactContext(ctx, string(raw))
		if err != nil {
			return err
		}
		var coordinates map[string]string
		if json.Unmarshal([]byte(redacted), &coordinates) != nil {
			return errors.New("cannot redact ledger coordinates")
		}
		value.Set(reflect.ValueOf(coordinates))
	case reflect.String:
		redacted, err := safety.NewRedactor().RedactContext(ctx, value.String())
		if err != nil {
			return err
		}
		value.SetString(redacted)
	}
	return ctx.Err()
}

func appendPrepared(ctx context.Context, conn *sql.Conn, prepared preparedEntry) (Entry, error) {
	entry := prepared.entry
	// The first operation reserves the write lock and sequence together.
	if err := conn.QueryRowContext(ctx, "UPDATE runs SET next_seq=next_seq+1 WHERE id=? RETURNING next_seq-1", entry.RunID).Scan(&entry.Seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, err
	}
	if entry.Seq <= 0 {
		return Entry{}, ErrCorrupt
	}
	existing, existingPayload, err := readEntry(conn.QueryRowContext(ctx, "SELECT id,run_id,seq,kind,recorded_at,payload_json FROM ledger_entries WHERE run_id=? AND id=?", entry.RunID, entry.ID))
	if err == nil {
		if existing.Kind != entry.Kind || !bytes.Equal(existing.Payload, entry.Payload) {
			return Entry{}, ErrConflict
		}
		for _, ref := range payloadReferences(existingPayload) {
			if err := checkReference(ctx, conn, existing, ref); err != nil {
				if errors.Is(err, ErrInvalidReference) {
					return Entry{}, ErrCorrupt
				}
				return Entry{}, err
			}
		}
		return existing, errReplay
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Entry{}, err
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO ledger_entries(run_id,seq,id,kind,recorded_at,payload_json) VALUES (?,?,?,?,?,?)", entry.RunID, entry.Seq, entry.ID, string(entry.Kind), entry.RecordedAt.Format(time.RFC3339Nano), string(entry.Payload)); err != nil {
		return Entry{}, err
	}
	for _, ref := range payloadReferences(prepared.payload) {
		if err := checkReference(ctx, conn, entry, ref); err != nil {
			return Entry{}, err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO ledger_refs(run_id,entry_id,target_id,relation) VALUES (?,?,?,?) ON CONFLICT DO NOTHING", entry.RunID, entry.ID, ref.target, ref.relation); err != nil {
			return Entry{}, err
		}
	}
	if metadata, ok := prepared.payload.(MetadataUpdate); ok && metadata.SessionID != nil {
		if _, err := conn.ExecContext(ctx, "INSERT INTO run_sessions(session_id,run_id) VALUES (?,?) ON CONFLICT(session_id) DO UPDATE SET run_id=excluded.run_id", *metadata.SessionID, entry.RunID); err != nil {
			return Entry{}, err
		}
	}
	return entry, nil
}

func checkReference(ctx context.Context, q Querier, entry Entry, ref ledgerReference) error {
	var kind Kind
	var raw string
	err := q.QueryRowContext(ctx, "SELECT kind,payload_json FROM ledger_entries WHERE run_id=? AND id=? AND seq<?", entry.RunID, ref.target, entry.Seq).Scan(&kind, &raw)
	if errors.Is(err, sql.ErrNoRows) || err == nil && kind != ref.kind {
		return ErrInvalidReference
	}
	if err != nil {
		return err
	}
	if _, err := DecodePayload(kind, json.RawMessage(raw)); err != nil {
		return ErrCorrupt
	}
	return nil
}

type ledgerReference struct {
	target   string
	kind     Kind
	relation string
}

func payloadReferences(payload any) []ledgerReference {
	var refs []ledgerReference
	add := func(kind Kind, relation string, ids ...string) {
		for _, id := range ids {
			if id != "" {
				refs = append(refs, ledgerReference{id, kind, relation})
			}
		}
	}
	switch p := payload.(type) {
	case Hypothesis:
		add(KindEvidence, "support", p.Supporting...)
		add(KindEvidence, "contradiction", p.Contradicting...)
		add(KindHypothesis, "supersession", p.Supersedes)
	case Option:
		add(KindHypothesis, "hypothesis", p.HypothesisIDs...)
	case Approval:
		add(KindOption, "option", p.OptionID)
	case Action:
		add(KindOption, "option", p.OptionID)
		add(KindApproval, "approval", p.ApprovalID)
		add(KindEvidence, "evidence", p.EvidenceIDs...)
	case Validation:
		add(KindExpectedBehavior, "criterion-set", p.ExpectedEntryID)
		add(KindAction, "action", p.ActionID)
		add(KindEvidence, "evidence", p.EvidenceIDs...)
	case SafetyViolation:
		add(KindSafetyViolation, "acknowledgment", p.Acknowledges)
	}
	return refs
}

func (s *Service) write(ctx context.Context, operation func(*sql.Conn) error) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	for _, pragma := range []struct {
		name  string
		value int
	}{{"foreign_keys", 1}, {"synchronous", 2}} {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA %s=%d", pragma.name, pragma.value)); err != nil {
			return err
		}
		var value int
		if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma.name).Scan(&value); err != nil {
			return err
		}
		if value != pragma.value {
			return errors.New("ledger connection durability setting was not applied")
		}
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, rollbackErr := conn.ExecContext(cleanup, "ROLLBACK"); rollbackErr != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			if err == nil {
				err = rollbackErr
			}
		}
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	if err := operation(conn); err != nil {
		if errors.Is(err, errReplay) {
			return nil
		}
		return err
	}
	if s.beforeCommit != nil {
		if err := s.beforeCommit(conn); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Service) Load(ctx context.Context, runID string) (Run, error) {
	if !validID(runID) {
		return Run{}, errors.New("invalid run identifier")
	}
	return s.read(ctx, func(Querier) (string, error) { return runID, nil })
}

func (s *Service) Latest(ctx context.Context) (Run, error) {
	return s.read(ctx, func(q Querier) (string, error) {
		var id string
		err := q.QueryRowContext(ctx, "SELECT id FROM runs ORDER BY creation_seq DESC LIMIT 1").Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("no runs recorded: %w", ErrNotFound)
		}
		return id, err
	})
}

func (s *Service) LookupSession(ctx context.Context, sessionID string) (Run, error) {
	if !validSessionID(sessionID) {
		return Run{}, errors.New("invalid session identifier")
	}
	return s.read(ctx, func(q Querier) (string, error) {
		var id string
		err := q.QueryRowContext(ctx, "SELECT run_id FROM run_sessions WHERE session_id=?", sessionID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return id, err
	})
}

func (s *Service) read(ctx context.Context, selectID func(Querier) (string, error)) (Run, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Run{}, ledgerError(ctx, "read run", err)
	}
	defer tx.Rollback()
	id, err := selectID(tx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Run{}, err
		}
		return Run{}, ledgerError(ctx, "select run", err)
	}
	run, err := LoadRun(ctx, tx, id)
	if err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, ledgerError(ctx, "finish snapshot", err)
	}
	return run, nil
}

// LoadRun uses the caller's transaction, including a later incident-capture write.
// Callers must supply one snapshot for all queries in this operation.
func LoadRun(ctx context.Context, q Querier, runID string) (Run, error) {
	if !validID(runID) {
		return Run{}, errors.New("invalid run identifier")
	}
	var problem, created string
	var next int64
	if err := q.QueryRowContext(ctx, "SELECT problem,created_at,next_seq FROM runs WHERE id=?", runID).Scan(&problem, &created, &next); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, ErrNotFound
		}
		return Run{}, ledgerError(ctx, "load run", err)
	}
	at, err := storedTime(created)
	if err != nil || !utf8.ValidString(problem) || next < 1 {
		return Run{}, ErrCorrupt
	}
	rows, err := q.QueryContext(ctx, "SELECT id,run_id,seq,kind,recorded_at,payload_json FROM ledger_entries WHERE run_id=? ORDER BY seq", runID)
	if err != nil {
		return Run{}, ledgerError(ctx, "load entries", err)
	}
	defer rows.Close()
	entries := []Entry{}
	prior := map[string]Kind{}
	for rows.Next() {
		entry, payload, err := readEntry(rows)
		if err != nil {
			return Run{}, ledgerError(ctx, "decode entry", err)
		}
		if entry.Seq != int64(len(entries)+1) {
			return Run{}, ErrCorrupt
		}
		for _, ref := range payloadReferences(payload) {
			if prior[ref.target] != ref.kind {
				return Run{}, ErrCorrupt
			}
		}
		prior[entry.ID] = entry.Kind
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return Run{}, ledgerError(ctx, "load entries", err)
	}
	if err := rows.Close(); err != nil {
		return Run{}, ledgerError(ctx, "close entries", err)
	}
	if next != int64(len(entries))+1 {
		return Run{}, ErrCorrupt
	}
	run, err := Reduce(runID, problem, at, entries)
	if err != nil {
		return Run{}, ErrCorrupt
	}
	if err := ctx.Err(); err != nil {
		return Run{}, err
	}
	return run, nil
}

type rowScanner interface{ Scan(...any) error }

func readEntry(row rowScanner) (Entry, any, error) {
	var entry Entry
	var recorded, raw string
	if err := row.Scan(&entry.ID, &entry.RunID, &entry.Seq, &entry.Kind, &recorded, &raw); err != nil {
		return Entry{}, nil, err
	}
	var err error
	entry.RecordedAt, err = storedTime(recorded)
	if err != nil || !validID(entry.ID) || !validID(entry.RunID) || entry.Seq <= 0 {
		return Entry{}, nil, ErrCorrupt
	}
	entry.Payload = json.RawMessage(raw)
	payload, err := DecodePayload(entry.Kind, entry.Payload)
	if err != nil {
		return Entry{}, nil, ErrCorrupt
	}
	return entry, payload, nil
}

func storedTime(raw string) (time.Time, error) {
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || !ledgerTimeFormat.MatchString(raw) || at.IsZero() || at.Location() != time.UTC {
		return time.Time{}, ErrCorrupt
	}
	return at, nil
}

func ledgerError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("ledger %s: %w", operation, ctx.Err())
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, ErrNotFound, ErrConflict, ErrInvalidReference, ErrCorrupt} {
		if errors.Is(err, known) {
			return fmt.Errorf("ledger %s: %w", operation, known)
		}
	}
	// Driver messages can contain payloads, identifiers and filesystem paths.
	return fmt.Errorf("ledger %s failed", operation)
}
