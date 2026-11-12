package ledger

import (
	"errors"
	"fmt"
	"time"
)

func Reduce(runID, problem string, createdAt time.Time, entries []Entry) (Run, error) {
	if !validID(runID) || createdAt.IsZero() {
		return Run{}, errors.New("invalid run identity or creation time")
	}
	run := Run{ID: runID, Problem: problem, CreatedAt: createdAt.UTC(), Entries: make([]Entry, 0, len(entries))}
	seen := make(map[string]Kind, len(entries))
	superseded := make(map[string]bool)
	for _, original := range entries {
		if !validID(original.ID) || original.RunID != runID || original.Seq <= run.ThroughSeq || original.RecordedAt.IsZero() {
			return Run{}, errors.New("invalid ledger entry identity, sequence or time")
		}
		if _, duplicate := seen[original.ID]; duplicate {
			return Run{}, errors.New("duplicate ledger entry identifier")
		}
		payload, err := DecodePayload(original.Kind, original.Payload)
		if err != nil {
			return Run{}, fmt.Errorf("invalid ledger entry at sequence %d: %w", original.Seq, err)
		}
		entry := copyEntry(original)
		switch value := payload.(type) {
		case MetadataUpdate:
			applyMetadata(&run, value)
		case Evidence:
			run.State.Evidence = append(run.State.Evidence, entry)
		case Hypothesis:
			if value.Supersedes != "" {
				if seen[value.Supersedes] != KindHypothesis {
					return Run{}, errors.New("hypothesis must supersede an earlier hypothesis in this run")
				}
				superseded[value.Supersedes] = true
			}
			run.State.Hypotheses = append(run.State.Hypotheses, entry)
		case ExpectedBehavior:
			run.State.Expected = &entry
		case Option:
			run.State.Options = append(run.State.Options, entry)
		case Approval:
			run.State.Approvals = append(run.State.Approvals, entry)
		case Action:
			run.State.Actions = append(run.State.Actions, entry)
		case Validation:
			run.State.Validations = append(run.State.Validations, entry)
		case SafetyViolation:
			run.State.SafetyViolations = append(run.State.SafetyViolations, entry)
		}
		seen[entry.ID] = entry.Kind
		run.ThroughSeq = entry.Seq
		run.Entries = append(run.Entries, copyEntry(original))
	}
	current := run.State.Hypotheses[:0]
	for _, hypothesis := range run.State.Hypotheses {
		if !superseded[hypothesis.ID] {
			current = append(current, hypothesis)
		}
	}
	run.State.Hypotheses = current
	return run, nil
}

func copyEntry(entry Entry) Entry {
	entry.Payload = append([]byte(nil), entry.Payload...)
	entry.RecordedAt = entry.RecordedAt.UTC()
	return entry
}

func applyMetadata(run *Run, update MetadataUpdate) {
	if update.Problem != nil {
		run.Problem = *update.Problem
	}
	if update.Phase != nil {
		run.State.Phase = *update.Phase
	}
	if update.Environment != nil {
		run.State.Environment = *update.Environment
	}
	if update.Tools != nil {
		run.State.Tools = *update.Tools
	}
	if update.Wave1 != nil {
		run.State.Wave1 = *update.Wave1
	}
	if update.Wave2 != nil {
		run.State.Wave2 = *update.Wave2
	}
	if update.Impact != nil {
		run.State.Impact = *update.Impact
	}
	if update.StartedAt != nil {
		run.State.StartedAt = update.StartedAt
	}
	if update.Detection != nil {
		run.State.Detection = *update.Detection
	}
	if update.Links != nil {
		run.State.Links = *update.Links
	}
}
