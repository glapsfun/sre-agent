package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Kind string

const (
	KindMetadata         Kind = "metadata"
	KindEvidence         Kind = "evidence"
	KindHypothesis       Kind = "hypothesis"
	KindExpectedBehavior Kind = "expected-behavior"
	KindOption           Kind = "option"
	KindApproval         Kind = "approval"
	KindAction           Kind = "action"
	KindValidation       Kind = "validation"
	KindSafetyViolation  Kind = "safety-violation"
	MaxPayloadBytes           = 64 << 10
)

type Entry struct {
	ID         string          `json:"id"`
	RunID      string          `json:"run_id"`
	Seq        int64           `json:"seq"`
	Kind       Kind            `json:"kind"`
	RecordedAt time.Time       `json:"recorded_at"`
	Payload    json.RawMessage `json:"payload"`
}

type MetadataUpdate struct {
	Problem     *string      `json:"problem,omitempty"`
	Phase       *int         `json:"phase,omitempty"`
	Environment *Environment `json:"environment,omitempty"`
	Tools       *Tools       `json:"tools,omitempty"`
	Wave1       *Wave        `json:"wave1,omitempty"`
	Wave2       *Wave        `json:"wave2,omitempty"`
	Impact      *string      `json:"impact,omitempty"`
	StartedAt   *time.Time   `json:"started_at,omitempty"`
	Detection   *string      `json:"detection,omitempty"`
	Links       *[]string    `json:"links,omitempty"`
	SessionID   *string      `json:"session_id,omitempty"`
}

type Evidence struct {
	Fact       string     `json:"fact"`
	Source     string     `json:"source"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	Links      []string   `json:"links,omitempty"`
}

type Hypothesis struct {
	Statement          string   `json:"statement"`
	Mechanism          string   `json:"mechanism"`
	Confidence         string   `json:"confidence"`
	Supporting         []string `json:"supporting"`
	Contradicting      []string `json:"contradicting"`
	DiscriminatingTest string   `json:"discriminating_test"`
	Status             string   `json:"status"`
	Supersedes         string   `json:"supersedes,omitempty"`
}

type ExpectedBehavior struct {
	Criteria []Criterion `json:"criteria"`
	Reason   string      `json:"reason,omitempty"`
}

type Option struct {
	Summary        string   `json:"summary"`
	Steps          []string `json:"steps"`
	Risk           string   `json:"risk"`
	Pros           []string `json:"pros"`
	Cons           []string `json:"cons"`
	ExpectedImpact string   `json:"expected_impact"`
	Rollback       string   `json:"rollback"`
	HypothesisIDs  []string `json:"hypothesis_ids,omitempty"`
}

type Approval struct {
	OptionID  string    `json:"option_id"`
	Decision  string    `json:"decision"`
	Statement string    `json:"statement"`
	At        time.Time `json:"at"`
	Target    string    `json:"target"`
}

type Action struct {
	At          time.Time `json:"at"`
	Command     string    `json:"command"`
	Result      string    `json:"result"`
	Effect      string    `json:"effect"`
	Status      string    `json:"status"`
	OptionID    string    `json:"option_id,omitempty"`
	ApprovalID  string    `json:"approval_id,omitempty"`
	EvidenceIDs []string  `json:"evidence_ids,omitempty"`
	Rollback    string    `json:"rollback,omitempty"`
}

type Validation struct {
	ExpectedEntryID string     `json:"expected_entry_id"`
	CriterionID     string     `json:"criterion_id"`
	ActionID        string     `json:"action_id"`
	Verdict         string     `json:"verdict"`
	Observed        string     `json:"observed"`
	Source          string     `json:"source"`
	EvidenceIDs     []string   `json:"evidence_ids"`
	At              time.Time  `json:"at"`
	WindowStart     *time.Time `json:"window_start,omitempty"`
	WindowEnd       *time.Time `json:"window_end,omitempty"`
	Reason          string     `json:"reason,omitempty"`
}

type SafetyViolation struct {
	Wave           int            `json:"wave"`
	Investigators  []string       `json:"investigators"`
	Changes        []SafetyChange `json:"changes"`
	Acknowledges   string         `json:"acknowledges,omitempty"`
	Acknowledgment string         `json:"acknowledgment,omitempty"`
}

type Environment struct {
	Context          string            `json:"context,omitempty"`
	Namespace        string            `json:"namespace,omitempty"`
	Workload         string            `json:"workload,omitempty"`
	GitOpsManager    string            `json:"gitops_manager,omitempty"`
	Cloud            string            `json:"cloud,omitempty"`
	SourceRepos      []Repository      `json:"source_repos,omitempty"`
	GitOpsRepos      []Repository      `json:"gitops_repos,omitempty"`
	CloudCoordinates map[string]string `json:"cloud_coordinates,omitempty"`
	Notes            []string          `json:"notes,omitempty"`
}

type Repository struct {
	Path   string `json:"path"`
	Remote string `json:"remote"`
}

type Tools struct {
	Available []string      `json:"available,omitempty"`
	Missing   []MissingTool `json:"missing,omitempty"`
}

type MissingTool struct {
	Name   string `json:"name"`
	Blocks string `json:"blocks"`
}

type Wave struct {
	Investigators []string `json:"investigators,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	MutationCheck string   `json:"mutation_check,omitempty"`
}

type Criterion struct {
	ID            string `json:"id"`
	Description   string `json:"description"`
	Observation   string `json:"observation"`
	Threshold     string `json:"threshold"`
	WindowSeconds *int64 `json:"window_seconds,omitempty"`
	Source        string `json:"source"`
}

type SafetyChange struct {
	Object     string `json:"object"`
	Field      string `json:"field"`
	BeforeHash string `json:"before_hash"`
	AfterHash  string `json:"after_hash"`
	Change     string `json:"change"`
}

type Run struct {
	ID         string    `json:"id"`
	Problem    string    `json:"problem"`
	CreatedAt  time.Time `json:"created_at"`
	ThroughSeq int64     `json:"through_seq"`
	Entries    []Entry   `json:"entries"`
	State      State     `json:"state"`
}

type State struct {
	Phase            int         `json:"phase"`
	Environment      Environment `json:"environment"`
	Tools            Tools       `json:"tools"`
	Wave1            Wave        `json:"wave1"`
	Wave2            Wave        `json:"wave2"`
	Impact           string      `json:"impact"`
	Detection        string      `json:"detection"`
	StartedAt        *time.Time  `json:"started_at"`
	Links            []string    `json:"links"`
	Evidence         []Entry     `json:"evidence"`
	Hypotheses       []Entry     `json:"hypotheses"`
	Options          []Entry     `json:"options"`
	Approvals        []Entry     `json:"approvals"`
	Actions          []Entry     `json:"actions"`
	Validations      []Entry     `json:"validations"`
	SafetyViolations []Entry     `json:"safety_violations"`
	Expected         *Entry      `json:"expected"`
}

type ValidationSummary struct {
	Verdict            string            `json:"verdict"`
	ExpectedEntryID    string            `json:"expected_entry_id"`
	ActionID           string            `json:"action_id"`
	Criteria           []CriterionResult `json:"criteria"`
	Reasons            []string          `json:"reasons"`
	EligibleForCapture bool              `json:"eligible_for_capture"`
}

type CriterionResult struct {
	CriterionID  string `json:"criterion_id"`
	ValidationID string `json:"validation_id"`
	Verdict      string `json:"verdict"`
	Observed     string `json:"observed"`
	Reason       string `json:"reason"`
}

type ValidatedRun struct {
	Run        Run               `json:"run"`
	Validation ValidationSummary `json:"validation"`
}

var ledgerID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var ledgerTimeFormat = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func validID(id string) bool { return ledgerID.MatchString(id) }

func validSessionID(id string) bool {
	return id != "" && len(id) <= 256 && utf8.ValidString(id) && strings.IndexFunc(id, unicode.IsControl) < 0
}

func DecodePayload(kind Kind, raw json.RawMessage) (any, error) {
	if len(raw) > MaxPayloadBytes {
		return nil, errors.New("ledger payload exceeds 64 KiB")
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("ledger payload must be UTF-8 JSON")
	}
	var payload any
	switch kind {
	case KindMetadata:
		payload = &MetadataUpdate{}
	case KindEvidence:
		payload = &Evidence{}
	case KindHypothesis:
		payload = &Hypothesis{}
	case KindExpectedBehavior:
		payload = &ExpectedBehavior{}
	case KindOption:
		payload = &Option{}
	case KindApproval:
		payload = &Approval{}
	case KindAction:
		payload = &Action{}
	case KindValidation:
		payload = &Validation{}
	case KindSafetyViolation:
		payload = &SafetyViolation{}
	default:
		return nil, errors.New("unsupported ledger entry kind")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(payload); err != nil {
		return nil, errors.New("invalid ledger payload JSON or field type")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("ledger payload must contain exactly one JSON object")
	}
	if err := checkPayloadShape(raw, reflect.TypeOf(payload).Elem()); err != nil {
		return nil, err
	}
	if err := uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return nil, err
	}
	if err := validatePayload(payload); err != nil {
		return nil, err
	}
	return reflect.ValueOf(payload).Elem().Interface(), nil
}

// Presence is checked separately because encoding/json accepts null scalars,
// absent fields and case-insensitive field names, all ambiguous in an update.
func checkPayloadShape(raw json.RawMessage, typ reflect.Type) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("ledger payload fields must not be null")
	}
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[time.Time]() {
		var value string
		if json.Unmarshal(raw, &value) != nil || !ledgerTimeFormat.MatchString(value) {
			return errors.New("ledger time must use RFC3339 with nanosecond precision or less")
		}
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return errors.New("ledger payload requires an object")
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name, optional, _ := strings.Cut(field.Tag.Get("json"), ",")
			value, exists := fields[name]
			if !exists {
				if optional != "omitempty" {
					return fmt.Errorf("required ledger field missing: %s", name)
				}
				continue
			}
			if err := checkPayloadShape(value, field.Type); err != nil {
				return err
			}
			if typ == reflect.TypeFor[Wave]() && name == "mutation_check" {
				var check string
				if json.Unmarshal(value, &check) != nil || !oneOf(check, "not_run", "verified", "inconclusive", "violation") {
					return errors.New("invalid wave mutation check")
				}
			}
			if optional == "omitempty" && field.Type.Kind() == reflect.String && oneOf(name, "supersedes", "option_id", "approval_id", "acknowledges") {
				var reference string
				if json.Unmarshal(value, &reference) != nil || !validID(reference) {
					return errors.New("invalid supplied ledger reference")
				}
			}
			delete(fields, name)
		}
		if len(fields) != 0 {
			return errors.New("unknown or incorrectly cased ledger field")
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return errors.New("ledger field requires an array")
		}
		for _, value := range values {
			if err := checkPayloadShape(value, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return errors.New("ledger field requires an object")
		}
		for _, value := range values {
			if err := checkPayloadShape(value, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func uniqueJSONKeys(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return errors.New("invalid ledger JSON")
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	keys := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			key, err := decoder.Token()
			if err != nil {
				return errors.New("invalid ledger JSON")
			}
			name := key.(string)
			if keys[name] {
				return errors.New("duplicate ledger JSON field")
			}
			keys[name] = true
		}
		if err := uniqueJSONKeys(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func validatePayload(payload any) error {
	invalid := errors.New("invalid ledger payload field")
	var references []string
	var optionalReferences []string
	var times []*time.Time
	switch value := payload.(type) {
	case *MetadataUpdate:
		if *value == (MetadataUpdate{}) {
			return errors.New("metadata update must supply at least one field")
		}
		if value.Phase != nil && (*value.Phase < 0 || *value.Phase > 6) {
			return errors.New("phase must be between 0 and 6")
		}
		if value.SessionID != nil {
			if !validSessionID(*value.SessionID) {
				return errors.New("invalid session identifier")
			}
		}
		for _, wave := range []*Wave{value.Wave1, value.Wave2} {
			if wave != nil && wave.MutationCheck != "" && !oneOf(wave.MutationCheck, "not_run", "verified", "inconclusive", "violation") {
				return errors.New("invalid wave mutation check")
			}
		}
		times = append(times, value.StartedAt)
	case *Evidence:
		times = append(times, value.ObservedAt)
	case *Hypothesis:
		if !oneOf(value.Confidence, "high", "med", "low") || !oneOf(value.Status, "open", "confirmed", "ruled_out", "failed") || (value.Confidence != "low" && len(value.Supporting) == 0) {
			return invalid
		}
		references = append(references, value.Supporting...)
		references = append(references, value.Contradicting...)
		optionalReferences = append(optionalReferences, value.Supersedes)
	case *ExpectedBehavior:
		if len(value.Criteria) == 0 {
			return errors.New("expected behavior requires criteria")
		}
		seen := map[string]bool{}
		for _, criterion := range value.Criteria {
			if !validID(criterion.ID) || seen[criterion.ID] || strings.TrimSpace(criterion.Description) == "" || strings.TrimSpace(criterion.Observation) == "" || (criterion.WindowSeconds != nil && *criterion.WindowSeconds <= 0) {
				return errors.New("invalid or duplicate expected criterion")
			}
			seen[criterion.ID] = true
		}
	case *Option:
		references = append(references, value.HypothesisIDs...)
	case *Approval:
		if !oneOf(value.Decision, "approved", "rejected", "revoked") {
			return invalid
		}
		references = append(references, value.OptionID)
		times = append(times, &value.At)
	case *Action:
		if !oneOf(value.Effect, "remediation", "rollback", "observation") || !oneOf(value.Status, "applied", "failed", "unverified", "rolled_back") {
			return invalid
		}
		references = append(references, value.EvidenceIDs...)
		optionalReferences = append(optionalReferences, value.OptionID, value.ApprovalID)
		times = append(times, &value.At)
	case *Validation:
		if !oneOf(value.Verdict, "pass", "fail", "pending", "unknown") {
			return invalid
		}
		references = append(references, value.ExpectedEntryID, value.CriterionID, value.ActionID)
		references = append(references, value.EvidenceIDs...)
		times = append(times, &value.At, value.WindowStart, value.WindowEnd)
	case *SafetyViolation:
		if (value.Wave != 1 && value.Wave != 2) || (value.Acknowledges == "" && len(value.Changes) == 0) {
			return invalid
		}
		optionalReferences = append(optionalReferences, value.Acknowledges)
		for _, change := range value.Changes {
			if !oneOf(change.Change, "added", "removed", "changed", "head") || (change.Change == "head" && (change.Object != "HEAD" || change.BeforeHash == "" || change.AfterHash == "")) {
				return invalid
			}
		}
	}
	for _, reference := range optionalReferences {
		if reference != "" {
			references = append(references, reference)
		}
	}
	for _, reference := range references {
		if !validID(reference) {
			return errors.New("invalid ledger identifier or reference")
		}
	}
	for _, at := range times {
		if at != nil {
			if at.IsZero() {
				return errors.New("ledger time must not be zero")
			}
			*at = at.UTC()
			if at.Year() < 0 || at.Year() > 9999 {
				return errors.New("ledger UTC time is outside the supported year range")
			}
		}
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
