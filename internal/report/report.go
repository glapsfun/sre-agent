// Package report projects one recorded investigation into a deterministic document.
package report

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
)

type Document struct {
	RunID      string   `json:"run_id"`
	ThroughSeq int64    `json:"through_seq"`
	Status     string   `json:"status"`
	Markdown   string   `json:"markdown"`
	Gaps       []string `json:"gaps"`
}

type RunReader interface {
	Load(context.Context, string) (ledger.Run, error)
}

func Generate(ctx context.Context, source RunReader, runID string) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	if strings.TrimSpace(runID) == "" {
		return Document{}, errors.New("report requires a run identifier")
	}
	run, err := source.Load(ctx, runID)
	if err != nil {
		return Document{}, sourceError{err}
	}
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	if run.ID != runID {
		return Document{}, errors.New("report source returned a different run")
	}
	return Build(run)
}

func Build(run ledger.Run) (Document, error) {
	summary := ledger.EvaluateValidation(run)
	document := Document{RunID: safety.NewRedactor().Redact(run.ID), ThroughSeq: run.ThroughSeq, Status: "unresolved", Gaps: []string{}}
	if summary.EligibleForCapture && summary.Verdict == "resolved" {
		document.Status = "resolved"
	}
	model := projection{problem: run.Problem, summary: summary, options: map[string]ledger.Option{}, validations: map[string]ledger.Validation{}}
	// Rebuild current fields from history so a stale or caller-modified State
	// cannot disagree with the evaluator's committed snapshot.
	snapshot, err := ledger.Reduce(run.ID, run.Problem, run.CreatedAt, run.Entries)
	if err != nil {
		document.Gaps = append(document.Gaps, "ledger details unavailable: invalid ledger snapshot")
	} else {
		model.problem = snapshot.Problem
		model.impact, model.detection, model.startedAt = snapshot.State.Impact, snapshot.State.Detection, snapshot.State.StartedAt
		model.links = append(model.links, snapshot.State.Links...)
		current := make(map[string]bool, len(snapshot.State.Hypotheses))
		for _, entry := range snapshot.State.Hypotheses {
			current[entry.ID] = true
		}
		for _, entry := range snapshot.Entries {
			payload, err := ledger.DecodePayload(entry.Kind, entry.Payload)
			if err != nil {
				return Document{}, ledger.ErrCorrupt
			}
			switch value := payload.(type) {
			case ledger.Evidence:
				value.Fact = summarize(value.Fact, "additional evidence detail omitted: "+entry.ID, &document.Gaps)
				model.evidence = append(model.evidence, record[ledger.Evidence]{entry.ID, entry.Seq, value})
				model.links = append(model.links, value.Links...)
			case ledger.Hypothesis:
				item := record[ledger.Hypothesis]{entry.ID, entry.Seq, value}
				if current[entry.ID] && value.Status == "confirmed" {
					model.causes = append(model.causes, item)
				}
				if value.Status == "ruled_out" || value.Status == "failed" {
					model.rejected = append(model.rejected, item)
				}
			case ledger.Option:
				model.options[entry.ID] = value
			case ledger.Action:
				value.Result = summarize(value.Result, "additional action result detail omitted: "+entry.ID, &document.Gaps)
				model.actions = append(model.actions, record[ledger.Action]{entry.ID, entry.Seq, value})
			case ledger.ExpectedBehavior:
				model.expectedID, model.expected = entry.ID, value
			case ledger.Validation:
				model.validations[entry.ID] = value
				model.results = append(model.results, record[ledger.Validation]{entry.ID, entry.Seq, value})
			}
		}
	}
	document.Markdown = safety.NewRedactor().Redact(render(document, model))
	for i, gap := range document.Gaps {
		document.Gaps[i] = safety.NewRedactor().Redact(gap)
	}
	return document, nil
}

type sourceError struct{ error }

func (err sourceError) Error() string { return safety.NewRedactor().Redact(err.error.Error()) }
func (err sourceError) Unwrap() error { return err.error }

type record[T any] struct {
	id    string
	seq   int64
	value T
}

type projection struct {
	problem, impact, detection string
	startedAt                  *time.Time
	summary                    ledger.ValidationSummary
	evidence                   []record[ledger.Evidence]
	causes, rejected           []record[ledger.Hypothesis]
	actions                    []record[ledger.Action]
	options                    map[string]ledger.Option
	expectedID                 string
	expected                   ledger.ExpectedBehavior
	results                    []record[ledger.Validation]
	validations                map[string]ledger.Validation
	links                      []string
}

func summarize(value, gap string, gaps *[]string) string {
	// Redact before splitting so multiline credentials cannot become fragments.
	multiline := strings.ContainsAny(value, "\r\n")
	value = safety.NewRedactor().Redact(value)
	if !strings.ContainsAny(value, "\r\n") {
		if multiline {
			// JSON redaction can collapse an entire dump onto one line. Its
			// original safe excerpt is ambiguous, so retain only the source.
			*gaps = append(*gaps, gap+" (redaction changed line structure; excerpt omitted)")
			return "detail omitted after redaction"
		}
		return value
	}
	*gaps = append(*gaps, gap)
	for _, line := range strings.FieldsFunc(value, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
