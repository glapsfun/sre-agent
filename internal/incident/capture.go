package incident

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/store"
)

type Service struct {
	db  *sql.DB
	now func() time.Time
}

func New(s *store.Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: s.DB(), now: now}
}

func (s *Service) Capture(ctx context.Context, opts CaptureOptions) (Incident, error) {
	if err := ctx.Err(); err != nil {
		return Incident{}, err
	}
	if strings.TrimSpace(opts.RunID) == "" {
		return Incident{}, errors.New("capture requires an explicit run identifier")
	}
	if err := validateSignature(opts.Service, opts.Symptom, opts.Fingerprint); err != nil {
		return Incident{}, err
	}
	redactor := safety.NewRedactor()
	var err error
	opts.Service, err = redactor.RedactContext(ctx, opts.Service)
	if err != nil {
		return Incident{}, err
	}
	opts.Fingerprint, err = redactor.RedactContext(ctx, opts.Fingerprint)
	if err != nil {
		return Incident{}, err
	}
	incident, err := s.capture(ctx, opts)
	if err != nil {
		if ctx.Err() != nil {
			return Incident{}, ctx.Err()
		}
		for _, known := range []error{context.Canceled, context.DeadlineExceeded, ledger.ErrNotFound, ledger.ErrNotValidated, ledger.ErrCorrupt, safety.ErrResponseTooLarge, safety.ErrResponseComplexity} {
			if errors.Is(err, known) {
				return Incident{}, fmt.Errorf("incident capture: %w", known)
			}
		}
		// Driver diagnostics may contain stored text, identifiers or paths.
		return Incident{}, errors.New("incident capture failed")
	}
	return incident, nil
}

func (s *Service) capture(ctx context.Context, opts CaptureOptions) (incident Incident, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Incident{}, err
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
			return Incident{}, err
		}
		var value int
		if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma.name).Scan(&value); err != nil {
			return Incident{}, err
		}
		if value != pragma.value {
			return Incident{}, errors.New("incident connection durability setting was not applied")
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
	// Reserve the writer before loading history so validation and persistence
	// cannot straddle an append, criteria replacement or approval revocation.
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Incident{}, err
	}
	run, err := ledger.LoadRun(ctx, conn, opts.RunID)
	if err != nil {
		return Incident{}, err
	}
	validated, err := ledger.ValidateCapture(run)
	if err != nil {
		return Incident{}, err
	}
	record, err := project(ctx, validated)
	if err != nil {
		return Incident{}, err
	}
	incident = Incident{RunID: run.ID, ThroughSeq: run.ThroughSeq, Service: opts.Service, Symptom: opts.Symptom, Fingerprint: opts.Fingerprint, Record: record}
	var previousSeq int64
	var previousTime, previousService, previousSymptom, previousFingerprint string
	err = conn.QueryRowContext(ctx, "SELECT through_seq,captured_at,service,symptom,fingerprint FROM incidents WHERE run_id=?", run.ID).Scan(
		&previousSeq, &previousTime, &previousService, &previousSymptom, &previousFingerprint)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Incident{}, err
	}
	unchanged := err == nil && previousSeq == incident.ThroughSeq && previousService == incident.Service && previousSymptom == incident.Symptom && previousFingerprint == incident.Fingerprint
	if unchanged {
		incident.CapturedAt, err = time.Parse(time.RFC3339Nano, previousTime)
		if err != nil || incident.CapturedAt.IsZero() || incident.CapturedAt.Location() != time.UTC {
			return Incident{}, ledger.ErrCorrupt
		}
	} else {
		incident.CapturedAt = s.now().UTC()
		if incident.CapturedAt.IsZero() || incident.CapturedAt.Year() < 0 || incident.CapturedAt.Year() > 9999 {
			return Incident{}, errors.New("invalid incident clock time")
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return Incident{}, err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO incidents(run_id,through_seq,captured_at,service,symptom,fingerprint,record_json)
VALUES (?,?,?,?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET through_seq=excluded.through_seq,captured_at=excluded.captured_at,
service=excluded.service,symptom=excluded.symptom,fingerprint=excluded.fingerprint,record_json=excluded.record_json`,
			incident.RunID, incident.ThroughSeq, incident.CapturedAt.Format(time.RFC3339Nano), incident.Service, incident.Symptom, incident.Fingerprint, string(raw)); err != nil {
			return Incident{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Incident{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Incident{}, err
	}
	committed = true
	return incident, nil
}

func project(ctx context.Context, validated ledger.ValidatedRun) (Record, error) {
	run := validated.Run
	record := Record{Problem: run.Problem, Impact: run.State.Impact, StartedAt: run.State.StartedAt, Environment: run.State.Environment,
		Hypotheses: []HypothesisRecord{}, Evidence: []EvidenceRecord{}, Results: []ValidationRecord{},
		Validation: validated.Validation, Links: run.State.Links, Gaps: []string{}}
	payloads := make(map[string]any, len(run.Entries))
	for _, entry := range run.Entries {
		if err := ctx.Err(); err != nil {
			return Record{}, err
		}
		payload, err := ledger.DecodePayload(entry.Kind, entry.Payload)
		if err != nil {
			return Record{}, ledger.ErrCorrupt
		}
		payloads[entry.ID] = payload
	}
	record.Fix = payloads[validated.Validation.ActionID].(ledger.Action)
	record.Expected = payloads[validated.Validation.ExpectedEntryID].(ledger.ExpectedBehavior)
	if record.Fix.OptionID != "" {
		option := payloads[record.Fix.OptionID].(ledger.Option)
		record.Option = &option
	}
	for _, entry := range run.State.Hypotheses {
		record.Hypotheses = append(record.Hypotheses, HypothesisRecord{ID: entry.ID, Hypothesis: payloads[entry.ID].(ledger.Hypothesis)})
	}
	for _, entry := range run.State.Evidence {
		record.Evidence = append(record.Evidence, EvidenceRecord{ID: entry.ID, Evidence: payloads[entry.ID].(ledger.Evidence)})
	}
	for _, criterion := range validated.Validation.Criteria {
		record.Results = append(record.Results, ValidationRecord{ID: criterion.ValidationID, Validation: payloads[criterion.ValidationID].(ledger.Validation)})
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return Record{}, err
	}
	redacted, err := safety.NewRedactor().RedactContext(ctx, string(raw))
	if err != nil {
		return Record{}, err
	}
	// Decode into an empty record: JSON decoding otherwise retains map keys
	// that the redactor renamed or removed.
	record = Record{}
	if err := safety.DecodeJSON(ctx, redacted, &record); err != nil {
		return Record{}, err
	}
	// Redact complete fields first, including multiline credentials, then
	// retain concise facts and describe omitted detail without copying it.
	for i := range record.Evidence {
		evidence := &record.Evidence[i]
		if summary, omitted := firstLine(evidence.Evidence.Fact); omitted {
			evidence.Evidence.Fact = summary
			record.Gaps = append(record.Gaps, "additional evidence detail omitted: "+evidence.ID)
		}
	}
	if summary, omitted := firstLine(record.Fix.Result); omitted {
		record.Fix.Result = summary
		record.Gaps = append(record.Gaps, "additional action result detail omitted: "+validated.Validation.ActionID)
	}
	return record, nil
}

func firstLine(text string) (string, bool) {
	if !strings.ContainsAny(text, "\r\n") {
		return text, false
	}
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line), true
		}
	}
	return "", true
}
