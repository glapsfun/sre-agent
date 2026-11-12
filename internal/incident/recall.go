package incident

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/signature"
)

// Rank orders candidates using heuristic weights, not causal confidence.
// It leaves the supplied incidents unchanged and does not apply the Top limit.
func Rank(query RecallOptions, incidents []Incident) []Match {
	queryTokens := fingerprintTokens(query.Fingerprint)
	matches := make([]Match, 0, len(incidents))
	for _, incident := range incidents {
		tier := 0
		serviceReason, symptomReason := "service differs", "symptom differs"
		if query.Service == incident.Service {
			tier += 2
			serviceReason = "service matches"
		}
		if query.Symptom == incident.Symptom {
			tier++
			symptomReason = "symptom matches"
		}
		candidateTokens := fingerprintTokens(incident.Fingerprint)
		similarity := 0.0
		if len(queryTokens) > 0 && len(candidateTokens) > 0 {
			intersection := 0
			for token := range queryTokens {
				if _, ok := candidateTokens[token]; ok {
					intersection++
				}
			}
			similarity = float64(intersection) / float64(len(queryTokens)+len(candidateTokens)-intersection)
		}
		matches = append(matches, Match{
			Incident: incident, Score: 100*tier + int(math.Round(99*similarity)), Similarity: similarity,
			Reasons: []string{serviceReason, symptomReason, fmt.Sprintf("normalized fingerprint similarity %.4f (ranking heuristic, not proof of the same cause)", similarity)},
		})
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Similarity != b.Similarity {
			return a.Similarity > b.Similarity
		}
		if !a.Incident.CapturedAt.Equal(b.Incident.CapturedAt) {
			return a.Incident.CapturedAt.After(b.Incident.CapturedAt)
		}
		return a.Incident.RunID < b.Incident.RunID
	})
	return matches
}

func fingerprintTokens(text string) map[string]struct{} {
	text = signature.Mask(safety.NewRedactor().Redact(text))
	tokens := make(map[string]struct{})
	for _, token := range strings.Fields(text) {
		tokens[token] = struct{}{}
	}
	return tokens
}

func (s *Service) Recall(ctx context.Context, opts RecallOptions) (RecallResult, error) {
	if err := ctx.Err(); err != nil {
		return RecallResult{}, err
	}
	if err := validateSignature(opts.Service, opts.Symptom, opts.Fingerprint); err != nil {
		return RecallResult{}, err
	}
	if opts.Top < 0 {
		return RecallResult{}, errors.New("top must be positive")
	}
	if opts.Top == 0 {
		opts.Top = 5
	}
	redactor := safety.NewRedactor()
	var err error
	opts.Service, err = redactor.RedactContext(ctx, opts.Service)
	if err != nil {
		return RecallResult{}, err
	}
	opts.Fingerprint, err = redactor.RedactContext(ctx, opts.Fingerprint)
	if err != nil {
		return RecallResult{}, err
	}
	incidents, err := s.readIncidents(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return RecallResult{}, ctx.Err()
		}
		for _, known := range []error{context.Canceled, context.DeadlineExceeded, ledger.ErrCorrupt, safety.ErrResponseTooLarge, safety.ErrResponseComplexity} {
			if errors.Is(err, known) {
				return RecallResult{}, fmt.Errorf("incident recall: %w", known)
			}
		}
		// Driver diagnostics may expose stored values or repository paths.
		return RecallResult{}, errors.New("incident recall failed")
	}
	matches := Rank(opts, incidents)
	if err := ctx.Err(); err != nil {
		return RecallResult{}, err
	}
	result := RecallResult{Matches: matches, Total: len(matches)}
	if result.Total == 0 {
		result.Message = "no incident history"
	} else if len(result.Matches) > opts.Top {
		result.Matches = result.Matches[:opts.Top]
	}
	return result, nil
}

func (s *Service) readIncidents(ctx context.Context) ([]Incident, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT run_id,through_seq,captured_at,service,symptom,fingerprint,record_json FROM incidents")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	incidents := []Incident{}
	redactor := safety.NewRedactor()
	for rows.Next() {
		var incident Incident
		var at, raw string
		if err := rows.Scan(&incident.RunID, &incident.ThroughSeq, &at, &incident.Service, &incident.Symptom, &incident.Fingerprint, &raw); err != nil {
			return nil, ledger.ErrCorrupt
		}
		incident.CapturedAt, err = time.Parse(time.RFC3339Nano, at)
		if err != nil || incident.CapturedAt.IsZero() || incident.CapturedAt.Location() != time.UTC || incident.ThroughSeq <= 0 || strings.TrimSpace(incident.RunID) == "" {
			return nil, ledger.ErrCorrupt
		}
		if err := validateSignature(incident.Service, incident.Symptom, incident.Fingerprint); err != nil {
			return nil, ledger.ErrCorrupt
		}
		for _, text := range []*string{&incident.Service, &incident.Fingerprint, &raw} {
			*text, err = redactor.RedactContext(ctx, *text)
			if err != nil {
				return nil, err
			}
		}
		var record *Record
		if err := safety.DecodeJSON(ctx, raw, &record); err != nil {
			for _, known := range []error{context.Canceled, context.DeadlineExceeded, safety.ErrResponseTooLarge, safety.ErrResponseComplexity} {
				if errors.Is(err, known) {
					return nil, err
				}
			}
			return nil, ledger.ErrCorrupt
		}
		// This marker is an invariant of captured projections. Recall does not
		// revalidate historical incidents against later ledger changes.
		if record == nil || !record.Validation.EligibleForCapture {
			return nil, ledger.ErrCorrupt
		}
		incident.Record = *record
		incidents = append(incidents, incident)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return incidents, nil
}

func validateSignature(service, symptom, fingerprint string) error {
	for _, field := range []struct{ name, value string }{{"service", service}, {"fingerprint", fingerprint}} {
		if !utf8.ValidString(field.value) || strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s must be nonempty UTF-8 text", field.name)
		}
	}
	switch symptom {
	case "crashloop", "latency", "errors", "metrics", "oom", "networking", "availability":
		return nil
	default:
		return errors.New("symptom must be one of: crashloop, latency, errors, metrics, oom, networking, availability")
	}
}
