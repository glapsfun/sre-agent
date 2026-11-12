package ledger

import (
	"errors"
	"fmt"
	"strings"
)

var ErrNotValidated = errors.New("ledger run is not validated")
var ErrInvalidValidation = errors.New("invalid ledger validation")

// EvaluateValidation derives the verdict from committed history, never narrative state.
func EvaluateValidation(run Run) ValidationSummary {
	snapshot, err := validationSnapshot(run)
	if err != nil {
		return ValidationSummary{Verdict: "unresolved", Reasons: []string{"invalid ledger snapshot"}}
	}
	return evaluateSnapshot(snapshot)
}

// ValidateCapture returns a detached snapshot. Capture must load, validate and
// insert within one caller-owned write transaction to serialize ledger changes.
func ValidateCapture(run Run) (ValidatedRun, error) {
	snapshot, err := validationSnapshot(run)
	if err != nil {
		return ValidatedRun{}, fmt.Errorf("%w: invalid ledger snapshot", ErrNotValidated)
	}
	summary := evaluateSnapshot(snapshot)
	if !summary.EligibleForCapture {
		return ValidatedRun{}, fmt.Errorf("%w: %s", ErrNotValidated, strings.Join(summary.Reasons, "; "))
	}
	return ValidatedRun{Run: snapshot, Validation: summary}, nil
}

func validationSnapshot(run Run) (Run, error) {
	if run.ThroughSeq != int64(len(run.Entries)) {
		return Run{}, ErrCorrupt
	}
	// A missing entry can hide a revocation without changing the last sequence.
	for i, entry := range run.Entries {
		if entry.Seq != int64(i)+1 {
			return Run{}, ErrCorrupt
		}
	}
	return Reduce(run.ID, run.Problem, run.CreatedAt, run.Entries)
}

func evaluateSnapshot(run Run) ValidationSummary {
	summary := ValidationSummary{Verdict: "unresolved", Criteria: []CriterionResult{}, Reasons: []string{}}
	entries := make(map[string]Entry, len(run.Entries))
	var actionEntry Entry
	var action Action
	for _, entry := range run.Entries {
		entries[entry.ID] = entry
		if entry.Kind == KindAction {
			payload, _ := DecodePayload(entry.Kind, entry.Payload)
			candidate := payload.(Action)
			if candidate.Effect == "remediation" || candidate.Effect == "rollback" {
				actionEntry, action = entry, candidate
			}
		}
	}
	summary.ActionID = actionEntry.ID
	expectedEntry := run.State.Expected
	if expectedEntry == nil {
		summary.Reasons = append(summary.Reasons, "no expected behavior recorded")
	}
	if actionEntry.ID == "" {
		summary.Reasons = append(summary.Reasons, "no remediation or rollback recorded")
	} else if action.Status != "applied" {
		summary.Reasons = append(summary.Reasons, "latest action is not applied")
	}
	approved := actionEntry.ID != "" && approvedAction(actionEntry, action, entries)
	if actionEntry.ID != "" && !approved {
		summary.Reasons = append(summary.Reasons, "latest action lacks a matching active approval")
	}
	if expectedEntry == nil {
		return summary
	}
	summary.ExpectedEntryID = expectedEntry.ID
	payload, _ := DecodePayload(expectedEntry.Kind, expectedEntry.Payload)
	expected := payload.(ExpectedBehavior)
	latest := make(map[string]Entry)
	for _, entry := range run.State.Validations {
		payload, _ := DecodePayload(entry.Kind, entry.Payload)
		v := payload.(Validation)
		if v.ExpectedEntryID == expectedEntry.ID && v.ActionID == actionEntry.ID {
			latest[v.CriterionID] = entry
		}
	}
	allPass, failed := true, false
	for _, criterion := range expected.Criteria {
		result := CriterionResult{CriterionID: criterion.ID, Verdict: "unknown", Reason: "no current validation recorded"}
		if entry, exists := latest[criterion.ID]; exists {
			payload, _ := DecodePayload(entry.Kind, entry.Payload)
			v := payload.(Validation)
			result.ValidationID, result.Observed = entry.ID, v.Observed
			err := checkValidationResult(entry, v, *expectedEntry, actionEntry)
			if err == nil {
				for _, ref := range payloadReferences(v) {
					if !earlierReference(entry, entries[ref.target], ref.kind) {
						err = ErrInvalidReference
						break
					}
				}
			}
			if err != nil {
				result.Reason = err.Error()
			} else {
				result.Verdict, result.Reason = v.Verdict, v.Reason
				if result.Verdict != "pass" && result.Reason == "" {
					result.Reason = "current validation is " + result.Verdict
				}
			}
		}
		summary.Criteria = append(summary.Criteria, result)
		if result.Verdict != "pass" {
			allPass = false
			summary.Reasons = append(summary.Reasons, criterion.ID+": "+result.Reason)
		}
		failed = failed || result.Verdict == "fail"
	}
	switch {
	case failed:
		summary.Verdict = "failed"
	case actionEntry.ID == "" || action.Status != "applied" || !approved:
	case allPass:
		summary.Verdict, summary.EligibleForCapture = "resolved", true
	default:
		summary.Verdict = "applied_but_unverified"
	}
	return summary
}

func earlierReference(entry, target Entry, kind Kind) bool {
	return target.ID != "" && target.RunID == entry.RunID && target.Kind == kind && target.Seq < entry.Seq
}

func approvedAction(entry Entry, action Action, entries map[string]Entry) bool {
	approvalEntry := entries[action.ApprovalID]
	if !earlierReference(entry, approvalEntry, KindApproval) {
		return false
	}
	payload, _ := DecodePayload(KindApproval, approvalEntry.Payload)
	approval := payload.(Approval)
	if approval.Decision != "approved" || approval.OptionID != action.OptionID || approval.At.After(action.At) || !earlierReference(approvalEntry, entries[action.OptionID], KindOption) {
		return false
	}
	for _, later := range entries {
		if later.Kind == KindApproval && later.Seq > approvalEntry.Seq {
			payload, _ := DecodePayload(KindApproval, later.Payload)
			p := payload.(Approval)
			if p.OptionID == action.OptionID && p.Decision == "revoked" {
				return false
			}
		}
	}
	return true
}

func checkValidationResult(entry Entry, v Validation, expectedEntry, actionEntry Entry) error {
	if !earlierReference(entry, expectedEntry, KindExpectedBehavior) || expectedEntry.ID != v.ExpectedEntryID || !earlierReference(entry, actionEntry, KindAction) || actionEntry.ID != v.ActionID {
		return ErrInvalidReference
	}
	expectedPayload, err := DecodePayload(KindExpectedBehavior, expectedEntry.Payload)
	if err != nil {
		return ErrCorrupt
	}
	actionPayload, err := DecodePayload(KindAction, actionEntry.Payload)
	if err != nil {
		return ErrCorrupt
	}
	var criterion *Criterion
	for _, candidate := range expectedPayload.(ExpectedBehavior).Criteria {
		if candidate.ID == v.CriterionID {
			criterion = &candidate
			break
		}
	}
	if criterion == nil {
		return ErrInvalidReference
	}
	invalid := func(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidValidation, reason) }
	if (v.Verdict == "pass" || v.Verdict == "fail") && (strings.TrimSpace(v.Observed) == "" || strings.TrimSpace(v.Source) == "") {
		return invalid("pass and fail require an observation and source")
	}
	action := actionPayload.(Action)
	if v.At.Before(action.At) || v.At.Before(expectedEntry.RecordedAt) {
		return invalid("observation predates the action or expected behavior")
	}
	if v.Verdict == "pass" && v.At.After(entry.RecordedAt) {
		return invalid("pass observation is in the future at recording")
	}
	if v.WindowStart != nil && v.WindowEnd != nil && v.WindowEnd.Before(*v.WindowStart) {
		return invalid("observation window is reversed")
	}
	if v.Verdict == "pass" && criterion.WindowSeconds != nil {
		if v.WindowStart == nil || v.WindowEnd == nil {
			return invalid("declared observation window requires both endpoints")
		}
		if v.WindowStart.Before(action.At) || v.WindowEnd.After(v.At) {
			return invalid("observation window is outside action and observation times")
		}
		// RFC3339 year bounds keep Unix-second subtraction safe; Duration would
		// overflow for large declared windows and saturate for long observations.
		seconds := v.WindowEnd.Unix() - v.WindowStart.Unix()
		if v.WindowEnd.Nanosecond() < v.WindowStart.Nanosecond() {
			seconds--
		}
		if seconds < *criterion.WindowSeconds {
			return invalid("observation window is shorter than the criterion requires")
		}
	}
	return nil
}
