package ledger

import (
	"errors"
	"strings"
)

var ErrInvalidInvestigation = errors.New("invalid investigation round transition or evidence")

type InvestigationUpdate struct {
	Number      int      `json:"number"`
	State       string   `json:"state"`
	Assessment  string   `json:"assessment,omitempty"`
	Reason      string   `json:"reason"`
	Checks      []string `json:"checks"`
	EvidenceIDs []string `json:"evidence_ids"`
	Questions   []string `json:"questions"`
}

type InvestigationState struct {
	Number      int      `json:"number"`
	Active      bool     `json:"active"`
	StartSeq    int64    `json:"start_seq"`
	Assessment  string   `json:"assessment,omitempty"`
	StopReason  string   `json:"stop_reason,omitempty"`
	Reason      string   `json:"reason"`
	Checks      []string `json:"checks"`
	EvidenceIDs []string `json:"evidence_ids"`
	Questions   []string `json:"questions"`
}

func validateInvestigation(update InvestigationUpdate) error {
	if update.Number < 0 || update.Number > 4 || !oneOf(update.State, "start", "finish") || strings.TrimSpace(update.Reason) == "" || len(update.Checks) == 0 {
		return ErrInvalidInvestigation
	}
	if update.State == "start" && update.Assessment != "" || update.State == "finish" && !oneOf(update.Assessment, "sufficient", "insufficient", "no_useful_data") {
		return ErrInvalidInvestigation
	}
	for _, text := range append(append([]string{}, update.Checks...), update.Questions...) {
		if strings.TrimSpace(text) == "" {
			return ErrInvalidInvestigation
		}
	}
	seen := map[string]bool{}
	for _, id := range update.EvidenceIDs {
		if !validID(id) || seen[id] {
			return ErrInvalidInvestigation
		}
		seen[id] = true
	}
	return nil
}

func applyInvestigation(run *Run, update InvestigationUpdate, seq int64) error {
	previous := run.State.Investigation
	if update.State == "start" {
		if previous == nil && update.Number != 0 || previous != nil && (previous.Active || previous.StopReason != "" || update.Number != previous.Number+1) {
			return ErrInvalidInvestigation
		}
	} else if previous == nil || !previous.Active || previous.Number != update.Number {
		return ErrInvalidInvestigation
	}
	startSeq := seq
	if update.State == "finish" {
		startSeq = previous.StartSeq
	}
	prior := make(map[string]Entry, len(run.State.Evidence))
	for _, entry := range run.State.Evidence {
		prior[entry.ID] = entry
	}
	useful := false
	for _, id := range update.EvidenceIDs {
		entry, ok := prior[id]
		if !ok {
			return ErrInvalidInvestigation
		}
		payload, err := DecodePayload(KindEvidence, entry.Payload)
		if err != nil {
			return err
		}
		value := payload.(Evidence)
		if value.Gap == "" && strings.TrimSpace(value.Fact) != "" && (update.Number == 0 || entry.Seq > startSeq) {
			useful = true
		}
	}
	state := &InvestigationState{Number: update.Number, Active: update.State == "start", StartSeq: startSeq, Assessment: update.Assessment, Reason: update.Reason, Checks: update.Checks, EvidenceIDs: update.EvidenceIDs, Questions: update.Questions}
	if !state.Active {
		switch {
		case update.Assessment == "sufficient":
			if !useful {
				return ErrInvalidInvestigation
			}
			state.StopReason = "sufficient_evidence"
		case update.Assessment == "no_useful_data" || !useful:
			state.StopReason = "no_useful_data"
		case update.Number == 4:
			state.StopReason = "round_limit"
		}
	}
	run.State.Investigation = state
	return nil
}
