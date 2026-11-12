package incident

import (
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
)

type CaptureOptions struct {
	RunID       string
	Service     string
	Symptom     string
	Fingerprint string
}

type RecallOptions struct {
	Service     string
	Symptom     string
	Fingerprint string
	Top         int
}

type Match struct {
	Incident   Incident `json:"incident"`
	Score      int      `json:"score"`
	Similarity float64  `json:"similarity"`
	Reasons    []string `json:"reasons"`
}

type RecallResult struct {
	Matches []Match `json:"matches"`
	Total   int     `json:"total"`
	Message string  `json:"message"`
}

type Incident struct {
	RunID       string    `json:"run_id"`
	ThroughSeq  int64     `json:"through_seq"`
	CapturedAt  time.Time `json:"captured_at"`
	Service     string    `json:"service"`
	Symptom     string    `json:"symptom"`
	Fingerprint string    `json:"fingerprint"`
	Record      Record    `json:"record"`
}

type Record struct {
	Problem     string                   `json:"problem"`
	Impact      string                   `json:"impact"`
	StartedAt   *time.Time               `json:"started_at"`
	Environment ledger.Environment       `json:"environment"`
	Hypotheses  []HypothesisRecord       `json:"hypotheses"`
	Evidence    []EvidenceRecord         `json:"evidence"`
	Fix         ledger.Action            `json:"fix"`
	Option      *ledger.Option           `json:"option"`
	Expected    ledger.ExpectedBehavior  `json:"expected"`
	Validation  ledger.ValidationSummary `json:"validation"`
	Results     []ValidationRecord       `json:"results"`
	Links       []string                 `json:"links"`
	Gaps        []string                 `json:"gaps"`
}

type HypothesisRecord struct {
	ID         string            `json:"id"`
	Hypothesis ledger.Hypothesis `json:"hypothesis"`
}

type EvidenceRecord struct {
	ID       string          `json:"id"`
	Evidence ledger.Evidence `json:"evidence"`
}

type ValidationRecord struct {
	ID         string            `json:"id"`
	Validation ledger.Validation `json:"validation"`
}
