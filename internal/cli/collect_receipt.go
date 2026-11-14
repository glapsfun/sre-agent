package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/collect"
	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
)

type collectionReceipt struct {
	Kind                  string                  `json:"kind"`
	RequestSHA256         string                  `json:"request_sha256"`
	RequestedMode         collect.Mode            `json:"requested_mode"`
	SelectedMode          collect.Mode            `json:"selected_mode"`
	Reason                string                  `json:"reason"`
	CodeSHA256            string                  `json:"code_sha256"`
	DurationMS            int64                   `json:"duration_ms"`
	Success               bool                    `json:"success"`
	RequiredKeysSatisfied bool                    `json:"required_keys_satisfied"`
	MissingRequired       []string                `json:"missing_required"`
	Calls                 []collectionCallReceipt `json:"calls"`
	GapCount              int                     `json:"gap_count"`
	Gaps                  []collect.Gap           `json:"gaps"`
	OmittedDetails        int                     `json:"omitted_details"`
}
type collectionCallReceipt struct {
	Key       string `json:"key"`
	SourceID  string `json:"source_id"`
	Tool      string `json:"tool"`
	Reference string `json:"reference"`
	Called    bool   `json:"called"`
	Success   bool   `json:"success"`
}

const collectionDetailsOmitted = "Collection receipt details omitted to fit the durable receipt budget."

func collectionIdentity(req collect.Request) string {
	program := sha256.Sum256([]byte(req.Program))
	raw, _ := json.Marshal(struct {
		Plan          collect.Plan
		Mode          collect.Mode
		ProgramSHA256 string
		CodeApproved  bool
		TimeoutNS     int64
	}{req.Plan, req.Mode, hex.EncodeToString(program[:]), req.CodeApproved, int64(req.Timeout)})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func projectCollection(req collect.Request, result collect.Result, success bool, digest string, at time.Time) (*collectionReceipt, ledger.Evidence, []byte, error) {
	receipt := &collectionReceipt{Kind: "collection_receipt_v1", RequestSHA256: digest, RequestedMode: result.RequestedMode, SelectedMode: result.SelectedMode, Reason: result.Reason, CodeSHA256: result.CodeSHA256, DurationMS: result.DurationMS, Success: success, RequiredKeysSatisfied: result.RequiredKeysSatisfied, MissingRequired: append([]string{}, result.MissingRequired...), Calls: []collectionCallReceipt{}, GapCount: len(result.Gaps), Gaps: append([]collect.Gap{}, result.Gaps...)}
	activities := map[string]collect.Activity{}
	for _, activity := range result.Activity {
		activities[activity.Key] = activity
	}
	for _, call := range req.Plan.Calls {
		c := collectionCallReceipt{Key: call.Key, SourceID: call.SourceID, Tool: call.Tool}
		if a, ok := activities[call.Key]; ok && a.SourceID == call.SourceID && a.Tool == call.Tool {
			c.Called, c.Success, c.Reference = true, a.Success, a.Reference
		}
		receipt.Calls = append(receipt.Calls, c)
	}
	// Redact the JSON object before sizing so live and durable receipts agree.
	raw, err := json.Marshal(receipt)
	if err != nil {
		return receipt, ledger.Evidence{}, nil, errors.New("collection receipt encoding failed")
	}
	if err = json.Unmarshal([]byte(safety.NewRedactor().Redact(string(raw))), receipt); err != nil {
		return receipt, ledger.Evidence{}, nil, errors.New("collection receipt redaction failed")
	}
	evidence := ledger.Evidence{Source: "collection"}
	at = at.UTC()
	if !at.IsZero() && at.Year() >= 0 && at.Year() <= 9999 {
		evidence.ObservedAt = &at
	}
	for {
		gaps := []string{}
		if !receipt.Success {
			gaps = append(gaps, "Collection incomplete.")
		}
		if receipt.GapCount > 0 {
			gaps = append(gaps, fmt.Sprintf("Collection recorded %d gaps.", receipt.GapCount))
		}
		if len(receipt.MissingRequired) > 0 {
			gaps = append(gaps, fmt.Sprintf("Collection missing %d required keys.", len(receipt.MissingRequired)))
		}
		if receipt.OmittedDetails > 0 {
			gaps = append(gaps, collectionDetailsOmitted)
		}
		evidence.Gap = strings.Join(gaps, " ")
		fact, encodeErr := json.Marshal(receipt)
		evidence.Fact = string(fact)
		payload, payloadErr := json.Marshal(evidence)
		if encodeErr == nil && payloadErr == nil && len(fact) <= 24<<10 && len(payload) <= ledger.MaxPayloadBytes {
			return receipt, evidence, payload, nil
		}
		removed := false
		for i := len(receipt.Calls) - 1; i >= 0; i-- {
			if receipt.Calls[i].Reference != "" {
				receipt.Calls[i].Reference = ""
				receipt.OmittedDetails++
				removed = true
				break
			}
		}
		if removed {
			continue
		}
		if len(receipt.Gaps) > 0 {
			receipt.Gaps = receipt.Gaps[:len(receipt.Gaps)-1]
			receipt.OmittedDetails++
			continue
		}
		return receipt, evidence, nil, errors.New("collection receipt exceeds durable budget")
	}
}

func decodeCollectionReceipt(fact string, req collect.Request, digest string) (*collectionReceipt, error) {
	invalid := func() (*collectionReceipt, error) { return nil, ledger.ErrConflict }
	if len(fact) > 24<<10 {
		return invalid()
	}
	decoder := json.NewDecoder(strings.NewReader(fact))
	if !uniqueCollectionJSON(decoder) {
		return invalid()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return invalid()
	}
	var receipt collectionReceipt
	decoder = json.NewDecoder(strings.NewReader(fact))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil {
		return invalid()
	}
	// Comparing decoded shapes catches omitted required fields and null scalars.
	canonical, err := json.Marshal(receipt)
	var original, roundtrip any
	if err != nil || json.Unmarshal([]byte(fact), &original) != nil || json.Unmarshal(canonical, &roundtrip) != nil || !reflect.DeepEqual(original, roundtrip) {
		return invalid()
	}
	if receipt.Kind != "collection_receipt_v1" || receipt.RequestSHA256 != digest || !collectionHash(receipt.RequestSHA256) || receipt.RequestedMode != req.Mode || receipt.DurationMS < 0 || receipt.GapCount < 0 || receipt.OmittedDetails < 0 || receipt.Calls == nil || receipt.Gaps == nil || receipt.MissingRequired == nil || len(receipt.Calls) != len(req.Plan.Calls) || receipt.GapCount < len(receipt.Gaps) {
		return invalid()
	}
	omittedGaps := receipt.GapCount - len(receipt.Gaps)
	if receipt.OmittedDetails < omittedGaps || receipt.OmittedDetails > omittedGaps+len(receipt.Calls) {
		return invalid()
	}
	switch receipt.SelectedMode {
	case collect.ModeNone:
		if receipt.Success || receipt.CodeSHA256 != "" || receipt.RequiredKeysSatisfied {
			return invalid()
		}
	case collect.ModeOrdinary:
		if req.Mode == collect.ModeCode || receipt.CodeSHA256 != "" {
			return invalid()
		}
	case collect.ModeCode:
		program := sha256.Sum256([]byte(req.Program))
		if req.Mode == collect.ModeOrdinary || !req.CodeApproved || strings.TrimSpace(req.Program) == "" || receipt.CodeSHA256 != hex.EncodeToString(program[:]) || !collectionHash(receipt.CodeSHA256) {
			return invalid()
		}
	default:
		return invalid()
	}
	if !validCollectionReason(receipt, req) {
		return invalid()
	}
	missing := map[string]bool{}
	next := 0
	for _, key := range receipt.MissingRequired {
		for next < len(req.Plan.Required) && req.Plan.Required[next] != key {
			next++
		}
		if next == len(req.Plan.Required) || missing[key] {
			return invalid()
		}
		next++
		missing[key] = true
	}
	if receipt.RequiredKeysSatisfied != (len(missing) == 0) || receipt.Success && (!receipt.RequiredKeysSatisfied || receipt.GapCount != 0) {
		return invalid()
	}
	emptyReferences := 0
	calls := map[string]collectionCallReceipt{}
	for i, c := range receipt.Calls {
		plan := req.Plan.Calls[i]
		if c.Key != plan.Key || c.SourceID != plan.SourceID || c.Tool != plan.Tool || !c.Called && (c.Success || c.Reference != "") || receipt.SelectedMode == collect.ModeNone && c.Called {
			return invalid()
		}
		if c.Reference == "" {
			emptyReferences++
		}
		calls[c.Key] = c
	}
	if receipt.OmittedDetails-omittedGaps > emptyReferences {
		return invalid()
	}
	for _, key := range req.Plan.Required {
		if !missing[key] && (!calls[key].Called || !calls[key].Success) {
			return invalid()
		}
	}
	for _, gap := range receipt.Gaps {
		if gap.Key != "" {
			c, ok := calls[gap.Key]
			if !ok || gap.SourceID != c.SourceID {
				return invalid()
			}
		}
	}
	return &receipt, nil
}

func collectionHash(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == value
}
func validCollectionReason(receipt collectionReceipt, req collect.Request) bool {
	switch receipt.Reason {
	case "":
		return receipt.SelectedMode == collect.ModeNone
	case "manual ordinary collection":
		return req.Mode == collect.ModeOrdinary && receipt.SelectedMode == collect.ModeOrdinary
	case "short or independent collection":
		return req.Mode == collect.ModeAuto && receipt.SelectedMode == collect.ModeOrdinary && (len(req.Plan.Calls) < 2 || req.Plan.Operation == "independent")
	case "eligible code collection":
		return receipt.SelectedMode == collect.ModeCode && (req.Mode == collect.ModeCode || req.Mode == collect.ModeAuto && len(req.Plan.Calls) >= 2 && req.Plan.Operation != "independent")
	case "code program missing", "code consent missing", "code source grant missing", "code credential unavailable", "code runtime unavailable":
		hasProgram := strings.TrimSpace(req.Program) != ""
		if receipt.Reason == "code program missing" && hasProgram || receipt.Reason != "code program missing" && !hasProgram {
			return false
		}
		if receipt.Reason == "code consent missing" && req.CodeApproved || receipt.Reason != "code program missing" && receipt.Reason != "code consent missing" && !req.CodeApproved {
			return false
		}
		return req.Mode == collect.ModeCode && receipt.SelectedMode == collect.ModeNone || req.Mode == collect.ModeAuto && receipt.SelectedMode == collect.ModeOrdinary && len(req.Plan.Calls) >= 2 && req.Plan.Operation != "independent"
	}
	return false
}

func uniqueCollectionJSON(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] {
				return false
			}
			seen[key] = true
		}
		if !uniqueCollectionJSON(decoder) {
			return false
		}
	}
	_, err = decoder.Token()
	return err == nil
}
