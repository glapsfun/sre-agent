package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/collect"
	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/store"
)

func TestCollectionReceiptBudgetAndRedaction(t *testing.T) {
	plan := collect.Plan{Operation: "join", Calls: []collect.Call{}, Required: []string{}}
	result := collect.Result{RequestedMode: collect.ModeAuto, SelectedMode: collect.ModeOrdinary, Reason: "code runtime unavailable", MissingRequired: []string{}, Gaps: []collect.Gap{}, Activity: []collect.Activity{}, RequiredKeysSatisfied: true}
	for i := 0; i < 32; i++ {
		key := fmt.Sprintf("%03d", i) + strings.Repeat("k", 125)
		source := strings.Repeat("s", 128)
		tool := strings.Repeat("t", 128)
		plan.Calls = append(plan.Calls, collect.Call{Key: key, SourceID: source, Tool: tool, Arguments: json.RawMessage(`{}`)})
		plan.Required = append(plan.Required, key)
		result.Activity = append(result.Activity, collect.Activity{Key: key, SourceID: source, Tool: tool, Reference: strings.Repeat("\x01", 2048), Success: true})
		result.Gaps = append(result.Gaps, collect.Gap{Key: key, SourceID: source, Reason: strings.Repeat("\x02", 2048), Impact: "password=private-value"})
	}
	req := collect.Request{Plan: plan, Mode: collect.ModeAuto, Program: "provided", CodeApproved: true, Timeout: 10 * time.Second}
	digest := collectionIdentity(req)
	receipt, evidence, raw, err := projectCollection(req, result, false, digest, time.Now().UTC())
	if err != nil || len(raw) > ledger.MaxPayloadBytes || len(evidence.Fact) > 24<<10 || receipt.OmittedDetails == 0 || receipt.GapCount != 32 || len(receipt.Calls) != 32 || evidence.Gap == "" {
		t.Fatalf("unbounded receipt: bytes=%d receipt=%+v err=%v", len(raw), receipt, err)
	}
	if strings.Contains(string(raw), "private-value") {
		t.Fatal("secret persisted")
	}
	redacted := safety.NewRedactor().Redact(evidence.Fact)
	decoded, err := decodeCollectionReceipt(redacted, req, digest)
	if err != nil || decoded.OmittedDetails != receipt.OmittedDetails || decoded.Success {
		t.Fatal("redacted receipt not replayable", err)
	}
	for i, c := range receipt.Calls {
		if c.Key != plan.Calls[i].Key || c.SourceID != plan.Calls[i].SourceID || c.Tool != plan.Calls[i].Tool || !c.Called || !c.Success {
			t.Fatal("identity lost", c)
		}
	}
	// Required keys are preserved even when the failed maximum-size plan never called anything.
	result.Activity = nil
	result.SelectedMode = collect.ModeNone
	result.Reason = ""
	result.MissingRequired = append([]string{}, plan.Required...)
	result.RequiredKeysSatisfied = false
	receipt, evidence, raw, err = projectCollection(req, result, false, digest, time.Now().UTC())
	if err != nil || len(raw) > ledger.MaxPayloadBytes || len(evidence.Fact) > 24<<10 || len(receipt.MissingRequired) != 32 {
		t.Fatal("minimal legal receipt does not fit", len(raw), err)
	}
}

func TestCollectionReceiptRejectsMalformedOutcome(t *testing.T) {
	plan, err := collect.DecodePlan([]byte(collectPlanJSON))
	if err != nil {
		t.Fatal(err)
	}
	req := collect.Request{Plan: plan, Mode: collect.ModeAuto, Timeout: 10 * time.Second}
	result := collect.Result{RequestedMode: collect.ModeAuto, SelectedMode: collect.ModeOrdinary, Reason: "short or independent collection", RequiredKeysSatisfied: true, Activity: []collect.Activity{{Key: "metric", SourceID: "metrics", Tool: "read", Success: true}}}
	digest := collectionIdentity(req)
	_, evidence, _, err := projectCollection(req, result, true, digest, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err := json.Unmarshal([]byte(evidence.Fact), &original); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		key   string
		value any
	}{
		{"kind", "collection_receipt_v2"}, {"request_sha256", strings.Repeat("a", 64)}, {"requested_mode", "code"}, {"selected_mode", "auto"}, {"code_sha256", "bad"}, {"duration_ms", -1}, {"gap_count", -1}, {"omitted_details", -1}, {"required_keys_satisfied", false}, {"missing_required", []string{"unknown"}}, {"calls", []any{}}, {"reason", "raw untrusted error"}, {"success", nil},
	} {
		fields := map[string]any{}
		for k, v := range original {
			fields[k] = v
		}
		fields[change.key] = change.value
		raw, _ := json.Marshal(fields)
		if _, err := decodeCollectionReceipt(string(raw), req, digest); err == nil {
			t.Fatal("malformed receipt accepted", change.key)
		}
	}
	delete(original, "success")
	raw, _ := json.Marshal(original)
	if _, err := decodeCollectionReceipt(string(raw), req, digest); err == nil {
		t.Fatal("missing required outcome accepted")
	}
	duplicate := strings.Replace(evidence.Fact, `"success":true`, `"success":false,"success":true`, 1)
	if _, err := decodeCollectionReceipt(duplicate, req, digest); err == nil {
		t.Fatal("duplicate outcome accepted")
	}
}

func TestCollectionReceiptSelectionMatchesRequest(t *testing.T) {
	plan, err := collect.DecodePlan([]byte(strings.Replace(collectionPlan("queue_depth"), "independent", "join", 1)))
	if err != nil {
		t.Fatal(err)
	}
	req := collect.Request{Plan: plan, Mode: collect.ModeAuto, Program: "provided", CodeApproved: true, Timeout: 10 * time.Second}
	result := collect.Result{RequestedMode: collect.ModeAuto, SelectedMode: collect.ModeOrdinary, Reason: "code runtime unavailable", RequiredKeysSatisfied: true, Activity: []collect.Activity{
		{Key: "mcp", SourceID: "mcp-metrics", Tool: "query", Success: true, Reference: "reference"},
		{Key: "native", SourceID: "utcp-metrics", Tool: "query", Success: true, Reference: "reference"},
	}}
	digest := collectionIdentity(req)
	_, evidence, _, err := projectCollection(req, result, true, digest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"code program missing", "code consent missing"} {
		fact := strings.Replace(evidence.Fact, "code runtime unavailable", reason, 1)
		if _, err := decodeCollectionReceipt(fact, req, digest); err == nil {
			t.Fatal("inconsistent selection reason accepted", reason)
		}
	}
	fact := strings.Replace(evidence.Fact, `"omitted_details":0`, `"omitted_details":1`, 1)
	if _, err := decodeCollectionReceipt(fact, req, digest); err == nil {
		t.Fatal("claimed omission with all references present")
	}
}

func TestCollectionMaximumReceiptDurableReplay(t *testing.T) {
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "maximum receipt", "--id", "run")
	plan := collect.Plan{Operation: "independent", Calls: []collect.Call{}, Required: []string{}}
	result := collect.Result{RequestedMode: collect.ModeAuto, SelectedMode: collect.ModeOrdinary, Reason: "short or independent collection", RequiredKeysSatisfied: true}
	for i := 0; i < 32; i++ {
		key := fmt.Sprintf("%03d", i) + strings.Repeat("k", 125)
		source := strings.Repeat("s", 128)
		tool := strings.Repeat("t", 128)
		plan.Calls = append(plan.Calls, collect.Call{Key: key, SourceID: source, Tool: tool, Arguments: json.RawMessage(`{}`)})
		plan.Required = append(plan.Required, key)
		result.Activity = append(result.Activity, collect.Activity{Key: key, SourceID: source, Tool: tool, Reference: strings.Repeat("\x01", 2048), Success: true})
	}
	input, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect.DecodePlan(input); err != nil {
		t.Fatal("maximum fixture is not legal", err)
	}
	req := collect.Request{Plan: plan, Mode: collect.ModeAuto, Timeout: 10 * time.Second}
	receipt, _, payload, err := projectCollection(req, result, true, collectionIdentity(req), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.OmittedDetails == 0 || !receipt.Success {
		t.Fatal("fixture did not omit successful receipt details")
	}
	db, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ledger.New(db, nil).Add(t.Context(), ledger.AddRequest{RunID: "run", ID: "entry", Kind: ledger.KindEvidence, Payload: payload})
	db.Close()
	if err != nil {
		t.Fatal("maximum receipt cannot commit", err)
	}
	code, out, gaps := collectionCLI(t, root, string(input), "entry", collect.Service{Runtime: forbiddenCollectRuntime{t}})
	if code != 0 || !out.Replayed || !out.Receipt.Success || out.Receipt.OmittedDetails != receipt.OmittedDetails || !strings.Contains(strings.Join(gaps, " "), collectionDetailsOmitted) {
		t.Fatal("redaction or omission broke replay", out, gaps)
	}
}

func TestCollectionReceiptRejectsCodeForIndependentAuto(t *testing.T) {
	plan, err := collect.DecodePlan([]byte(collectPlanJSON))
	if err != nil {
		t.Fatal(err)
	}
	req := collect.Request{Plan: plan, Mode: collect.ModeAuto, Program: "provided", CodeApproved: true, Timeout: 10 * time.Second}
	program := sha256.Sum256([]byte(req.Program))
	result := collect.Result{RequestedMode: collect.ModeAuto, SelectedMode: collect.ModeCode, CodeSHA256: hex.EncodeToString(program[:]), Reason: "eligible code collection", RequiredKeysSatisfied: true, Activity: []collect.Activity{{Key: "metric", SourceID: "metrics", Tool: "read", Success: true}}}
	digest := collectionIdentity(req)
	_, evidence, _, err := projectCollection(req, result, true, digest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCollectionReceipt(evidence.Fact, req, digest); err == nil {
		t.Fatal("auto-independent code selection accepted")
	}
}
