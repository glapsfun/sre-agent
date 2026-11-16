package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/collect"
	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/sources"
	"github.com/glapsfun/sre-agent/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Synthetic provider data is deliberately separate from the literal oracle below.
func incidentFixtureData(scenario, source string) json.RawMessage {
	switch scenario + "/" + source {
	case "latency-dependency/telemetry":
		return json.RawMessage(`{"synthetic":true,"service":"checkout","dependency":"inventory","p95_ms":900,"baseline_ms":100}`)
	case "latency-dependency/context":
		return json.RawMessage(`{"synthetic":true,"service":"inventory","p95_ms":750}`)
	case "deployment-errors/telemetry":
		return json.RawMessage(`{"synthetic":true,"service":"checkout","revision":"rev-b","errors_before":2,"errors_after":32,"observed_at":"2026-11-16T10:01:00Z"}`)
	case "deployment-errors/context":
		return json.RawMessage(`{"synthetic":true,"service":"checkout","revision":"rev-b","previous_revision":"rev-a","deployed_at":"2026-11-16T10:00:00Z"}`)
	case "unavailable-source/telemetry":
		return json.RawMessage(`{"synthetic":true,"service":"checkout","p95_ms":600}`)
	}
	return nil
}

func incidentProvider(t *testing.T, id string, measure *sourceFixtureMeasurements) sources.Source {
	t.Helper()
	credential := "fixture-" + id + "-read-credential"
	env := "SRE_INCIDENT_" + strings.ToUpper(id) + "_TOKEN"
	t.Setenv(env, credential)
	schema := map[string]any{"type": "object", "properties": map[string]any{"scenario": map[string]string{"type": "string"}, "read_only": map[string]string{"type": "boolean"}}, "required": []string{"scenario", "read_only"}, "additionalProperties": false}
	server := mcp.NewServer(&mcp.Implementation{Name: "incident-" + id, Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "read", InputSchema: schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Scenario string `json:"scenario"`
			ReadOnly bool   `json:"read_only"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil || !args.ReadOnly {
			return nil, fmt.Errorf("read-only intent required")
		}
		measure.recordCall(id, args.Scenario)
		data := incidentFixtureData(args.Scenario, id)
		if len(data) == 0 {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{}}, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: data}, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("fixture request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		request := &measuredSourceRequest{path: r.URL.Path, remote: r.RemoteAddr, protocol: r.ProtoMajor, requestBytes: len(r.URL.RequestURI()) + len(raw)}
		measure.mu.Lock()
		measure.requests[id] = append(measure.requests[id], request)
		measure.mu.Unlock()
		w = &measuredSourceResponse{ResponseWriter: w, measure: measure, request: request}
		switch r.URL.Path {
		case "/mcp":
			if r.Header.Get("Authorization") != "Bearer "+credential {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			mcpHandler.ServeHTTP(w, r)
		case "/native":
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("Authorization") != "" {
				t.Error("credential exposed to native runtime request")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			scenario := r.URL.Query().Get("scenario")
			measure.recordCall("native-"+id, scenario)
			data := incidentFixtureData(scenario, id)
			if len(data) == 0 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(httpServer.Close)
	return sources.Source{ID: id, Name: "Synthetic " + id, Kind: "mcp-http", Endpoint: httpServer.URL + "/mcp", CredentialEnv: env, AccessRef: "fixture/read-only-tools", Reference: httpServer.URL + "/mcp"}
}

func incidentCollectionFixture(t *testing.T) (string, *sourceFixtureMeasurements, map[string]string) {
	t.Helper()
	root := ledgerRepo(t)
	ledgerData(t, root, "", "run", "start", "synthetic incident comparison", "--id", "run")
	measure := &sourceFixtureMeasurements{requests: map[string][]*measuredSourceRequest{}, calls: map[string]int{}}
	known := []sources.Source{incidentProvider(t, "telemetry", measure), incidentProvider(t, "context", measure)}
	input, err := json.Marshal(map[string]any{"sources": known})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "known.json")
	if err := os.WriteFile(path, input, 0600); err != nil {
		t.Fatal(err)
	}
	scout, err := (sources.Service{}).Scout(t.Context(), root, sources.ScoutOptions{Approved: true, KnownFile: path})
	if err != nil || len(scout.Candidates) != 2 {
		t.Fatalf("scout: %+v %v", scout, err)
	}
	selected := []sources.Source{}
	references := map[string]string{}
	for _, candidate := range scout.Candidates {
		if candidate.Status != "ready" || len(candidate.Tools) != 1 {
			t.Fatalf("candidate not ready: %+v", candidate)
		}
		source := candidate.Source
		endpoint, err := url.Parse(source.Endpoint)
		if err != nil {
			t.Fatal(err)
		}
		source.CodeNetwork = &sources.CodeNetwork{HostPort: endpoint.Host, AccessRef: "fixture/read-only-tools-and-no-anonymous-writes"}
		selected = append(selected, source)
		references[source.ID] = source.Reference
	}
	if _, err := sources.Approve(t.Context(), root, sources.ApprovalRequest{Approved: true, CodeNetworkApproved: true, Sources: selected}); err != nil {
		t.Fatal(err)
	}
	return root, measure, references
}

func incidentPlan(scenario string) collect.Plan {
	args, _ := json.Marshal(map[string]any{"scenario": scenario, "read_only": true})
	return collect.Plan{Operation: "join", Required: []string{"primary", "secondary"}, Calls: []collect.Call{
		{Key: "primary", SourceID: "telemetry", Tool: "read", Arguments: args},
		{Key: "secondary", SourceID: "context", Tool: "read", Arguments: args},
	}}
}

func incidentProgram(scenario string, references map[string]string) string {
	urls := map[string]string{}
	for source, reference := range references {
		urls[source] = strings.TrimSuffix(reference, "/mcp") + "/native?scenario=" + url.QueryEscape(scenario)
	}
	encoded, _ := json.Marshal(urls)
	return `if (collection.phase === "source") {
  const urls = ` + string(encoded) + `;
  const values = [];
  for (const key of collection.keys) {
    const helper = await collection.call(key.key);
    if (!helper.ok) continue;
    const response = await fetch(urls[collection.source_id]);
    if (!response.ok) throw new Error("native read failed");
    const record = await response.json();
    const toolRecord = helper.result.data.structuredContent;
    if (Object.keys(record).length !== Object.keys(toolRecord).length || Object.entries(record).some(([key,value]) => toolRecord[key] !== value)) throw new Error("inconsistent source views");
    values.push({key:key.key, source_id:key.source_id, data:{record}});
  }
  return values;
}
const values = collection.values;
const primary = values.find(value => value.key === "primary");
const secondary = values.find(value => value.key === "secondary");
if (primary && secondary) {
  const a = primary.data.record, b = secondary.data.record;
  const association = {source_id:secondary.source_id, key:secondary.key, record:b, conclusion:"association_only"};
  if (a.dependency === b.service) association.slowdown = a.p95_ms / a.baseline_ms;
  if (a.service === b.service && a.revision === b.revision) association.increase = a.errors_after - a.errors_before;
  primary.data.association = association;
}
return values;`
}

// This oracle consumes only live evidence and dynamic endpoint references. Its
// factual expectations never use the provider fixture or program builder.
func checkIncidentEvidence(scenario string, result collect.Result, references map[string]string) error {
	var primaryJSON, secondaryJSON string
	var derivedName string
	var derivedValue float64
	switch scenario {
	case "latency-dependency":
		primaryJSON = `{"synthetic":true,"service":"checkout","dependency":"inventory","p95_ms":900,"baseline_ms":100}`
		secondaryJSON = `{"synthetic":true,"service":"inventory","p95_ms":750}`
		derivedName, derivedValue = "slowdown", 9
	case "deployment-errors":
		primaryJSON = `{"synthetic":true,"service":"checkout","revision":"rev-b","errors_before":2,"errors_after":32,"observed_at":"2026-11-16T10:01:00Z"}`
		secondaryJSON = `{"synthetic":true,"service":"checkout","revision":"rev-b","previous_revision":"rev-a","deployed_at":"2026-11-16T10:00:00Z"}`
		derivedName, derivedValue = "increase", 30
	case "unavailable-source":
		primaryJSON = `{"synthetic":true,"service":"checkout","p95_ms":600}`
	default:
		return fmt.Errorf("unknown incident %q", scenario)
	}
	if result.SelectedMode != collect.ModeOrdinary && result.SelectedMode != collect.ModeCode {
		return fmt.Errorf("no collection selected")
	}
	wantOrigin := "source"
	if result.SelectedMode == collect.ModeCode {
		wantOrigin = "program_derived"
	}
	wants := map[string]string{"primary": primaryJSON}
	if secondaryJSON != "" {
		wants["secondary"] = secondaryJSON
	}
	if len(result.Evidence) != len(wants) {
		return fmt.Errorf("evidence count: got %d want %d", len(result.Evidence), len(wants))
	}
	seen := map[string]bool{}
	records := map[string]map[string]any{}
	var association map[string]any
	for _, evidence := range result.Evidence {
		expected, ok := wants[evidence.Key]
		id := "telemetry"
		if evidence.Key == "secondary" {
			id = "context"
		}
		if !ok || seen[evidence.Key] || evidence.SourceID != id || evidence.Tool != "read" || evidence.Reference != references[id] || evidence.Reference == "" || evidence.Origin != wantOrigin || evidence.At.IsZero() || evidence.At.Location() != time.UTC {
			return fmt.Errorf("invalid evidence provenance for %q", evidence.Key)
		}
		seen[evidence.Key] = true
		var payload map[string]any
		if err := json.Unmarshal(evidence.Data, &payload); err != nil {
			return err
		}
		var record map[string]any
		if result.SelectedMode == collect.ModeOrdinary {
			record, ok = payload["structuredContent"].(map[string]any)
			if payload["isError"] == true {
				return fmt.Errorf("error presented as evidence")
			}
		} else {
			record, ok = payload["record"].(map[string]any)
			allowedFields := 1
			if evidence.Key == "primary" && secondaryJSON != "" {
				association, _ = payload["association"].(map[string]any)
				allowedFields = 2
			}
			if len(payload) != allowedFields {
				return fmt.Errorf("unexpected derived fields for %s", evidence.Key)
			}
		}
		var want map[string]any
		if err := json.Unmarshal([]byte(expected), &want); err != nil {
			return err
		}
		if !ok || !reflect.DeepEqual(record, want) {
			return fmt.Errorf("incorrect facts for %s: %s", evidence.Key, evidence.Data)
		}
		records[evidence.Key] = record
	}
	if secondaryJSON != "" {
		// The ordinary records must independently support the same arithmetic and
		// association as the explicit code join. Neither asserts confirmed cause.
		a, b := records["primary"], records["secondary"]
		if scenario == "latency-dependency" && (a["dependency"] != b["service"] || a["p95_ms"].(float64)/a["baseline_ms"].(float64) != 9) {
			return fmt.Errorf("invalid latency association")
		}
		if scenario == "deployment-errors" {
			if a["service"] != b["service"] || a["revision"] != b["revision"] || a["errors_after"].(float64)-a["errors_before"].(float64) != 30 {
				return fmt.Errorf("invalid deployment association")
			}
			observed, e1 := time.Parse(time.RFC3339, a["observed_at"].(string))
			deployed, e2 := time.Parse(time.RFC3339, b["deployed_at"].(string))
			if e1 != nil || e2 != nil || observed.Sub(deployed) != time.Minute {
				return fmt.Errorf("invalid deployment order")
			}
		}
		if result.SelectedMode == collect.ModeCode {
			want := map[string]any{"source_id": "context", "key": "secondary", "record": b, "conclusion": "association_only", derivedName: derivedValue}
			if !reflect.DeepEqual(association, want) {
				return fmt.Errorf("incorrect offline join: %v", association)
			}
		}
	}
	if len(result.Activity) != 2 {
		return fmt.Errorf("missing source activity")
	}
	seen = map[string]bool{}
	for _, activity := range result.Activity {
		id, success := "telemetry", true
		if activity.Key == "secondary" {
			id, success = "context", secondaryJSON != ""
		} else if activity.Key != "primary" {
			return fmt.Errorf("unknown activity")
		}
		if seen[activity.Key] || activity.SourceID != id || activity.Tool != "read" || activity.Reference != references[id] || activity.Reference == "" || activity.At.IsZero() || activity.At.Location() != time.UTC || activity.ElapsedMS < 0 || activity.Success != success {
			return fmt.Errorf("incorrect activity: %+v", activity)
		}
		seen[activity.Key] = true
		for _, evidence := range result.Evidence {
			if evidence.Key == activity.Key && !evidence.At.Equal(activity.At) {
				return fmt.Errorf("evidence timestamp does not match helper activity")
			}
		}
		if success && len(activity.Gaps) != 0 {
			return fmt.Errorf("unexpected activity gap")
		}
		if !success && (len(activity.Gaps) != 1 || activity.Gaps[0].SourceID != "context" || activity.Gaps[0].Reason != "source collection failed" || activity.Gaps[0].Impact != "Source evidence is unavailable; the tool reported a collection failure.") {
			return fmt.Errorf("failed activity lost meaningful gap")
		}
	}
	if secondaryJSON == "" {
		if result.RequiredKeysSatisfied || !slices.Equal(result.MissingRequired, []string{"secondary"}) || len(result.Gaps) != 2 {
			return fmt.Errorf("unavailable source falsely resolved")
		}
		want := []collect.Gap{
			{Key: "secondary", SourceID: "context", Reason: "source collection failed", Impact: "Source evidence is unavailable; the tool reported a collection failure."},
			{Key: "secondary", SourceID: "context", Reason: "required evidence missing", Impact: "Collection evidence is incomplete."},
		}
		if !reflect.DeepEqual(result.Gaps, want) {
			return fmt.Errorf("unavailable source lost reason/impact: %+v", result.Gaps)
		}
	} else if !result.RequiredKeysSatisfied || len(result.MissingRequired) != 0 || len(result.Gaps) != 0 {
		return fmt.Errorf("complete incident lost coverage")
	}
	return nil
}

func checkIncidentInterval(result collect.Result, start, finish time.Time) error {
	for _, evidence := range result.Evidence {
		if evidence.At.Before(start) || evidence.At.After(finish) {
			return fmt.Errorf("evidence timestamp outside collection interval")
		}
	}
	for _, activity := range result.Activity {
		if activity.At.Before(start) || activity.At.After(finish) {
			return fmt.Errorf("activity timestamp outside collection interval")
		}
	}
	for _, audit := range result.Audit {
		if audit.At.Location() != time.UTC || audit.At.Before(start) || audit.At.After(finish) {
			return fmt.Errorf("audit timestamp outside collection interval")
		}
	}
	return nil
}

func incidentOracleMutations(t *testing.T, scenario string, result collect.Result, references map[string]string) {
	t.Helper()
	clone := func() collect.Result {
		raw, _ := json.Marshal(result)
		var copy collect.Result
		if err := json.Unmarshal(raw, &copy); err != nil {
			t.Fatal(err)
		}
		return copy
	}
	reject := func(name string, mutated collect.Result) {
		t.Helper()
		if err := checkIncidentEvidence(scenario, mutated, references); err == nil {
			t.Fatalf("oracle accepted mutation %s", name)
		}
	}
	for i := range result.Evidence {
		mutated := clone()
		mutated.Evidence = append(mutated.Evidence[:i], mutated.Evidence[i+1:]...)
		reject("remove-"+result.Evidence[i].Key, mutated)
		mutated = clone()
		// Coverage and provenance stay intact while the factual payload changes.
		mutated.Evidence[i].Data = bytes.ReplaceAll(mutated.Evidence[i].Data, []byte(`"service":"`), []byte(`"service":"wrong-`))
		reject("fact-with-valid-keys-"+result.Evidence[i].Key, mutated)
		for _, field := range []string{"source", "tool", "reference", "origin", "time"} {
			mutated = clone()
			switch field {
			case "source":
				mutated.Evidence[i].SourceID = "wrong"
			case "tool":
				mutated.Evidence[i].Tool = "wrong"
			case "reference":
				mutated.Evidence[i].Reference = "wrong"
			case "origin":
				mutated.Evidence[i].Origin = "program_only"
			case "time":
				mutated.Evidence[i].At = time.Time{}
			}
			reject(field+"-"+result.Evidence[i].Key, mutated)
		}
	}
	mutated := clone()
	mutated.Activity[0].Success = false
	reject("activity-outcome", mutated)
	if scenario == "unavailable-source" {
		mutated = clone()
		mutated.Gaps = nil
		reject("erased-gap", mutated)
		mutated = clone()
		mutated.Gaps = nil
		mutated.MissingRequired = nil
		mutated.RequiredKeysSatisfied = true
		mutated.Activity[1].Success = true
		mutated.Activity[1].Gaps = nil
		reject("falsely-resolved-gap", mutated)
		mutated = clone()
		mutated.Gaps[0].Impact = ""
		reject("erased-impact", mutated)
	} else {
		mutated = clone()
		mutated.RequiredKeysSatisfied = false
		reject("coverage", mutated)
		if result.SelectedMode == collect.ModeCode {
			mutated = clone()
			var payload map[string]any
			if err := json.Unmarshal(mutated.Evidence[0].Data, &payload); err != nil {
				t.Fatal(err)
			}
			association := payload["association"].(map[string]any)
			if scenario == "latency-dependency" {
				association["slowdown"] = 8
			} else {
				association["increase"] = 29
			}
			mutated.Evidence[0].Data, _ = json.Marshal(payload)
			reject("incorrect-derived-value", mutated)
			mutated = clone()
			if err := json.Unmarshal(mutated.Evidence[0].Data, &payload); err != nil {
				t.Fatal(err)
			}
			association = payload["association"].(map[string]any)
			association["conclusion"] = "confirmed_cause"
			mutated.Evidence[0].Data, _ = json.Marshal(payload)
			reject("invented-cause", mutated)
		}
	}
}

func TestBunMCPIncidentComparison(t *testing.T) {
	requireCLIBun(t)
	root, measure, references := incidentCollectionFixture(t)
	t.Run("native routes reject mutations", func(t *testing.T) {
		for _, reference := range references {
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodConnect, http.MethodTrace} {
				req, err := http.NewRequestWithContext(t.Context(), method, strings.TrimSuffix(reference, "/mcp")+"/native", nil)
				if err != nil {
					t.Fatal(err)
				}
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				if response.StatusCode != http.StatusMethodNotAllowed {
					t.Fatalf("native %s accepted: %d", method, response.StatusCode)
				}
			}
		}
	})
	for _, scenario := range []string{"latency-dependency", "deployment-errors", "unavailable-source"} {
		t.Run(scenario, func(t *testing.T) {
			planBytes, err := json.Marshal(incidentPlan(scenario))
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []collect.Mode{collect.ModeOrdinary, collect.ModeCode} {
				t.Run(string(mode), func(t *testing.T) {
					program := ""
					args := []string{"--mode", string(mode)}
					if mode == collect.ModeCode {
						program = incidentProgram(scenario, references)
						path := filepath.Join(root, "program.js")
						if err := os.WriteFile(path, []byte(program), 0600); err != nil {
							t.Fatal(err)
						}
						args = append(args, "--program", path, "--approve-code")
					}
					beforeRequests, beforeCalls, beforeNative := map[string][]measuredSourceRequest{}, map[string]int{}, map[string]int{}
					for _, id := range []string{"telemetry", "context"} {
						beforeRequests[id], beforeCalls[id] = measure.snapshot(id, scenario)
						_, beforeNative[id] = measure.snapshot("native-"+id, scenario)
					}
					start := time.Now()
					exit, output := publicCollectionCLI(t, root, string(planBytes), scenario+"-"+string(mode), args...)
					finish := time.Now()
					wantExit := 0
					if scenario == "unavailable-source" {
						wantExit = 1
					}
					if exit != wantExit || output.Result == nil || output.Receipt == nil || !output.Committed || output.Receipt.Success != (wantExit == 0) {
						t.Fatalf("collection outcome: exit=%d result=%+v receipt=%+v", exit, output.Result, output.Receipt)
					}
					result := *output.Result
					if result.SelectedMode != mode {
						t.Fatalf("selected %s want %s", result.SelectedMode, mode)
					}
					if err := checkIncidentEvidence(scenario, result, references); err != nil {
						t.Fatalf("oracle: %v; result=%+v", err, result)
					}
					if err := checkIncidentInterval(result, start, finish); err != nil {
						t.Fatal(err)
					}
					incidentOracleMutations(t, scenario, result, references)
					requests, requestBytes, responseBytes, nativeReads := 0, 0, 0, 0
					for _, id := range []string{"telemetry", "context"} {
						after, calls := measure.snapshot(id, scenario)
						if calls-beforeCalls[id] != 1 {
							t.Fatalf("%s helper calls=%d", id, calls-beforeCalls[id])
						}
						_, native := measure.snapshot("native-"+id, scenario)
						wantNative := 0
						if mode == collect.ModeCode && (scenario != "unavailable-source" || id == "telemetry") {
							wantNative = 1
						}
						if native-beforeNative[id] != wantNative {
							t.Fatalf("%s native reads=%d want %d", id, native-beforeNative[id], wantNative)
						}
						nativeReads += wantNative
						for _, request := range after[len(beforeRequests[id]):] {
							requests++
							requestBytes += request.requestBytes
							responseBytes += request.responseBytes
						}
						audits := 0
						endpoint, _ := url.Parse(references[id])
						for _, event := range result.Audit {
							if event.SourceID == id {
								if event.Phase != "source" || event.Permission != "network-allowed" || event.Value != endpoint.Host {
									t.Fatalf("unexpected proxy audit: %+v", event)
								}
								audits++
							}
						}
						if audits != wantNative {
							t.Fatalf("%s proxy observations=%d want %d", id, audits, wantNative)
						}
					}
					if len(result.Audit) != nativeReads {
						t.Fatalf("unexpected join or source audit: %+v", result.Audit)
					}
					if result.InputTokens != nil || result.OutputTokens != nil || result.CostUSD != nil || result.UsageReason != "No model request was made." {
						t.Fatal("invented model usage")
					}
					live, err := json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
					row, err := json.Marshal(map[string]any{
						"scenario": scenario, "mode": mode, "plan_bytes": len(planBytes), "program_bytes": len([]byte(program)), "supplied_input_bytes": len(planBytes) + len([]byte(program)), "live_result_bytes": len(live),
						"fixture_requests": requests, "fixture_request_body_and_uri_bytes": requestBytes, "fixture_response_body_bytes": responseBytes, "native_reads": nativeReads, "wall_us": finish.Sub(start).Microseconds(),
						"evidence_complete": result.RequiredKeysSatisfied, "gaps": len(result.Gaps), "oracle_passed": true, "input_tokens": nil, "output_tokens": nil, "cost_usd": nil, "usage_reason": "No model request was made.",
					})
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("INCIDENT_MEASUREMENT %s", row)
				})
			}
		})
	}
	t.Run("source phase cannot read other source", func(t *testing.T) {
		plan, err := json.Marshal(incidentPlan("latency-dependency"))
		if err != nil {
			t.Fatal(err)
		}
		target := strings.TrimSuffix(references["context"], "/mcp") + "/native?scenario=latency-dependency"
		encoded, _ := json.Marshal(target)
		path := filepath.Join(root, "denied.js")
		if err := os.WriteFile(path, []byte(`await fetch(`+string(encoded)+`); return [];`), 0600); err != nil {
			t.Fatal(err)
		}
		before, _ := measure.snapshot("context", "latency-dependency")
		exit, output := publicCollectionCLI(t, root, string(plan), "cross-source-denied", "--mode", "code", "--program", path, "--approve-code")
		after, _ := measure.snapshot("context", "latency-dependency")
		if exit != 1 || output.Result == nil || output.Result.SelectedMode != collect.ModeCode || len(before) != len(after) {
			t.Fatalf("cross-source request escaped: exit=%d result=%+v", exit, output.Result)
		}
		denied := false
		endpoint, _ := url.Parse(references["context"])
		for _, event := range output.Result.Audit {
			denied = denied || event.Phase == "source" && event.SourceID == "telemetry" && event.Permission == "network-denied" && event.Value == endpoint.Host
		}
		if !denied {
			t.Fatalf("missing host-observed denial: %+v", output.Result.Audit)
		}
	})
	db, err := store.OpenReadOnly(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	run, err := ledger.New(db, nil).Load(t.Context(), "run")
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range run.Entries {
		for _, forbidden := range []string{"collection.phase", "fixture-telemetry-read-credential", "fixture-context-read-credential", "structuredContent", "p95_ms", "errors_after", "association_only", "network-allowed", "network-denied", "audit", "input_tokens"} {
			if bytes.Contains(entry.Payload, []byte(forbidden)) {
				t.Fatalf("raw incident data in durable receipt: %q", forbidden)
			}
		}
	}
}
