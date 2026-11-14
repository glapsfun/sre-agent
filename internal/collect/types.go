// Package collect collects external, untrusted evidence through approved sources.
package collect

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/glapsfun/sre-agent/internal/sources"
)

type Mode string

const (
	ModeAuto     Mode = "auto"
	ModeOrdinary Mode = "ordinary"
	ModeCode     Mode = "code"
	ModeNone     Mode = "none"
)

var (
	ErrInvalidPlan          = errors.New("invalid collection plan")
	ErrCodeUnavailable      = errors.New("code collection unavailable")
	ErrIncompleteCollection = errors.New("incomplete collection")
)

type Call struct {
	Key       string          `json:"key"`
	SourceID  string          `json:"source_id"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}
type Plan struct {
	Calls     []Call   `json:"calls"`
	Required  []string `json:"required"`
	Operation string   `json:"operation"`
}
type Request struct {
	Plan         Plan
	Mode         Mode
	Program      string
	CodeApproved bool
	Timeout      time.Duration
}
type Value struct {
	Key      string          `json:"key"`
	SourceID string          `json:"source_id"`
	Data     json.RawMessage `json:"data"`
}
type Evidence struct {
	Key       string          `json:"key"`
	SourceID  string          `json:"source_id"`
	Tool      string          `json:"tool"`
	Reference string          `json:"reference"`
	At        time.Time       `json:"at"`
	Data      json.RawMessage `json:"data"`
	Origin    string          `json:"origin"`
}
type Activity struct {
	Key       string        `json:"key"`
	SourceID  string        `json:"source_id"`
	Tool      string        `json:"tool"`
	Reference string        `json:"reference"`
	At        time.Time     `json:"at"`
	ElapsedMS int64         `json:"elapsed_ms"`
	Success   bool          `json:"success"`
	Gaps      []sources.Gap `json:"gaps"`
}
type Gap struct {
	Key      string `json:"key"`
	SourceID string `json:"source_id"`
	Reason   string `json:"reason"`
	Impact   string `json:"impact"`
}
type AuditEvent struct {
	Phase      string    `json:"phase"`
	SourceID   string    `json:"source_id"`
	At         time.Time `json:"at"`
	Permission string    `json:"permission"`
	Value      string    `json:"value"`
}
type Key struct {
	Key      string `json:"key"`
	SourceID string `json:"source_id"`
	Tool     string `json:"tool"`
}
type Invocation struct {
	Phase    string  `json:"phase"`
	SourceID string  `json:"source_id"`
	HostPort string  `json:"host_port"`
	Program  string  `json:"program"`
	Keys     []Key   `json:"keys"`
	Values   []Value `json:"values"`
}
type Execution struct {
	Values []Value      `json:"values"`
	Audit  []AuditEvent `json:"audit"`
}
type Helper func(string) (sources.CallResult, error)
type Runtime interface {
	Probe(context.Context) error
	Open(context.Context) (Session, error)
}
type Session interface {
	Run(context.Context, Invocation, Helper) (Execution, error)
	Close(context.Context) error
}
type Service struct {
	Sources sources.Service
	Runtime Runtime
}
type Result struct {
	RequestedMode         Mode         `json:"requested_mode"`
	SelectedMode          Mode         `json:"selected_mode"`
	Reason                string       `json:"reason"`
	CodeSHA256            string       `json:"code_sha256"`
	DurationMS            int64        `json:"duration_ms"`
	Evidence              []Evidence   `json:"evidence"`
	Activity              []Activity   `json:"activity"`
	Audit                 []AuditEvent `json:"audit"`
	Gaps                  []Gap        `json:"gaps"`
	MissingRequired       []string     `json:"missing_required"`
	RequiredKeysSatisfied bool         `json:"required_keys_satisfied"`
	InputTokens           *int64       `json:"input_tokens"`
	OutputTokens          *int64       `json:"output_tokens"`
	CostUSD               *string      `json:"cost_usd"`
	UsageReason           string       `json:"usage_reason"`
}
