package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/sources"
	"github.com/glapsfun/sre-agent/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func prometheusFixture(query string) map[string]any {
	values := map[string]string{"queue_depth": "12", "http_errors_total": "27"}
	return map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{map[string]any{"metric": map[string]string{"__name__": query}, "value": []any{1794672000, values[query]}}}}}
}

func utcpComparisonServer(t *testing.T, measure *sourceFixtureMeasurements) (string, []sources.Source) {
	t.Helper()
	t.Setenv("SRE_FIXTURE_READ_TOKEN", "fixture-read-credential")
	schema := map[string]any{"type": "object", "properties": map[string]any{"query": map[string]string{"type": "string"}}, "required": []string{"query"}, "additionalProperties": false}
	server := mcp.NewServer(&mcp.Implementation{Name: "prometheus-fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "query", InputSchema: schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &args)
		measure.recordCall("mcp-metrics", args.Query)
		if args.Query == "unavailable" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "metrics unavailable"}}}, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: prometheusFixture(args.Query)}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	var base string
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read fixture request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		id := "utcp-metrics"
		if r.URL.Path == "/mcp" {
			id = "mcp-metrics"
		}
		request := &measuredSourceRequest{path: r.URL.Path, remote: r.RemoteAddr, protocol: r.ProtoMajor, requestBytes: len(r.URL.RequestURI()) + len(raw)}
		measure.mu.Lock()
		measure.requests[id] = append(measure.requests[id], request)
		measure.mu.Unlock()
		w = &measuredSourceResponse{ResponseWriter: w, measure: measure, request: request}
		if r.Header.Get("Authorization") != "Bearer fixture-read-credential" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path == "/mcp" {
			handler.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/utcp":
			_ = json.NewEncoder(w).Encode(map[string]any{"utcp_version": "1.1.0", "manual_version": "1.0.0", "tools": []any{map[string]any{"name": "query", "description": "Query fixture metrics", "inputs": schema, "tool_call_template": map[string]any{"call_template_type": "http", "url": base + "/api/v1/query", "http_method": "GET", "auth": map[string]string{"auth_type": "api_key", "api_key": "Bearer ${SRE_FIXTURE_READ_TOKEN}", "var_name": "Authorization", "location": "header"}}}}})
		case "/api/v1/query":
			measure.recordCall("utcp-metrics", r.URL.Query().Get("query"))
			if r.URL.Query().Get("query") == "unavailable" {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"metrics unavailable"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(prometheusFixture(r.URL.Query().Get("query")))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	base = httpServer.URL
	t.Cleanup(httpServer.Close)
	return base, []sources.Source{
		{ID: "mcp-metrics", Name: "MCP metrics", Kind: "mcp-http", Endpoint: base + "/mcp", CredentialEnv: "SRE_FIXTURE_READ_TOKEN", AccessRef: "fixture provider rejects writes", Tools: []sources.Tool{}},
		{ID: "utcp-metrics", Name: "UTCP metrics", Kind: "utcp-http", Endpoint: base + "/utcp", CredentialEnv: "SRE_FIXTURE_READ_TOKEN", AccessRef: "fixture provider rejects writes", Tools: []sources.Tool{}},
	}
}

type measuredSourceRequest struct {
	path, remote                string
	protocol                    int
	requestBytes, responseBytes int
}

type sourceFixtureMeasurements struct {
	mu       sync.Mutex
	requests map[string][]*measuredSourceRequest
	calls    map[string]int
}

func (m *sourceFixtureMeasurements) recordCall(id, query string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[id+"/"+query]++
}

func (m *sourceFixtureMeasurements) snapshot(id, query string) ([]measuredSourceRequest, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	requests := make([]measuredSourceRequest, len(m.requests[id]))
	for i, request := range m.requests[id] {
		requests[i] = *request
	}
	return requests, m.calls[id+"/"+query]
}

type measuredSourceResponse struct {
	http.ResponseWriter
	measure *sourceFixtureMeasurements
	request *measuredSourceRequest
}

func (w *measuredSourceResponse) Write(raw []byte) (int, error) {
	w.measure.mu.Lock()
	defer w.measure.mu.Unlock()
	n, err := w.ResponseWriter.Write(raw)
	w.request.responseBytes += n
	return n, err
}

func (w *measuredSourceResponse) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *measuredSourceResponse) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *measuredSourceResponse) Flush() {
	_ = w.FlushError()
}

func TestUTCPNativeComparisonAndCLIReceipts(t *testing.T) {
	root := ledgerRepo(t)
	measure := &sourceFixtureMeasurements{requests: map[string][]*measuredSourceRequest{}, calls: map[string]int{}}
	base, known := utcpComparisonServer(t, measure)
	references := map[string]string{"mcp-metrics": base + "/mcp", "utcp-metrics": base + "/utcp"}
	const failureReason = "source collection failed"
	const nativeFailureImpact = "Source evidence is unavailable; the investigation remains incomplete."
	const mcpFailureImpact = "Source evidence is partial and may be incomplete; the tool reported a collection failure."
	input, _ := json.Marshal(map[string]any{"sources": known})
	path := filepath.Join(root, "known.json")
	if err := os.WriteFile(path, input, 0600); err != nil {
		t.Fatal(err)
	}
	scout, err := (sources.Service{}).Scout(t.Context(), root, sources.ScoutOptions{Approved: true, KnownFile: path})
	if err != nil || len(scout.Candidates) != 2 {
		t.Fatalf("manual discovery failed: %+v %v", scout, err)
	}
	selected := []sources.Source{}
	for _, candidate := range scout.Candidates {
		if candidate.Status != "ready" || len(candidate.Tools) != 1 {
			t.Fatalf("not ready: %+v", candidate)
		}
		selected = append(selected, candidate.Source)
	}
	approval, _ := json.Marshal(map[string]any{"sources": selected})
	if code, _, raw := sourceCLI(t, root, string(approval), "approve", "--approve", "--file", "-"); code != 0 {
		t.Fatal(raw)
	}
	for _, scenario := range []string{"queue_depth", "http_errors_total", "unavailable"} {
		for _, id := range []string{"mcp-metrics", "utcp-metrics"} {
			args, _ := json.Marshal(map[string]string{"query": scenario})
			beforeRequests, beforeCalls := measure.snapshot(id, scenario)
			start := time.Now()
			result, callErr := (sources.Service{}).Call(t.Context(), root, sources.CallOptions{SourceID: id, Tool: "query", Arguments: args})
			finish := time.Now()
			elapsed := finish.Sub(start)
			afterRequests, afterCalls := measure.snapshot(id, scenario)
			requests := afterRequests[len(beforeRequests):]
			if result.SourceID != id || result.Reference != references[id] || result.Tool != "query" || result.At.IsZero() || result.At.Location() != time.UTC || result.At.Before(start) || result.At.After(finish) {
				t.Fatalf("lost source provenance: %s %s %+v", scenario, id, result)
			}
			if got := afterCalls - beforeCalls; got != 1 {
				t.Fatalf("fixture query was not collected exactly once: %s %s calls=%d", scenario, id, got)
			}
			if id == "utcp-metrics" && (len(requests) != 2 || requests[0].path != "/utcp" || requests[1].path != "/api/v1/query" || requests[0].remote == requests[1].remote || requests[0].protocol != 1 || requests[1].protocol != 1) {
				t.Fatalf("native collection did not use separate HTTP/1 connections for manual and query: %s %+v", scenario, requests)
			}
			complete := false
			if scenario == "unavailable" {
				impact := nativeFailureImpact
				if id == "mcp-metrics" {
					impact = mcpFailureImpact
					var response mcp.CallToolResult
					if err := json.Unmarshal(result.Data, &response); err != nil || !response.IsError || len(response.Content) != 1 {
						t.Fatalf("lost partial failure evidence: %+v %v", response, err)
					}
					content, ok := response.Content[0].(*mcp.TextContent)
					if !ok || content.Text != "metrics unavailable" {
						t.Fatalf("wrong partial failure evidence: %+v", response.Content)
					}
				} else if len(result.Data) != 0 {
					t.Fatalf("unexpected evidence from unavailable native source: %s", result.Data)
				}
				complete = callErr != nil && callErr.Error() == failureReason && len(result.Gaps) == 1 && result.Gaps[0] == (sources.Gap{SourceID: id, Reason: failureReason, Impact: impact})
			} else {
				data := result.Data
				if id == "mcp-metrics" {
					var wrapped struct {
						Data json.RawMessage `json:"structuredContent"`
					}
					if err := json.Unmarshal(data, &wrapped); err != nil {
						t.Fatal(err)
					}
					data = wrapped.Data
				}
				var actual any
				_ = json.Unmarshal(data, &actual)
				canonical, _ := json.Marshal(actual)
				want, _ := json.Marshal(prometheusFixture(scenario))
				complete = callErr == nil && bytes.Equal(canonical, want) && len(result.Gaps) == 0
			}
			if !complete {
				t.Fatalf("lost key evidence or gap: %s %s %+v %v", scenario, id, result, callErr)
			}
			var requestBytes, responseBytes int
			for _, request := range requests {
				requestBytes += request.requestBytes
				responseBytes += request.responseBytes
			}
			record, _ := json.Marshal(map[string]any{"scenario": scenario, "transport": id, "evidence_complete": complete, "requests": len(requests), "request_body_and_uri_bytes": requestBytes, "response_body_bytes": responseBytes, "result_bytes": len(result.Data), "latency_us": elapsed.Microseconds(), "model_tokens": nil, "model_cost": nil, "usage_gap": "No model request; tokens and cost were not measured."})
			t.Log(string(record))
		}
	}
	ledgerData(t, root, "", "run", "start", "native metrics", "--id", "utcp-run")
	cliStart := time.Now()
	if code, _, raw := sourceCLI(t, root, `{"query":"queue_depth"}`, "call", "utcp-metrics", "query", "--args", "-", "--run", "utcp-run", "--id", "native"); code != 0 || !strings.Contains(raw, "queue_depth") || !strings.Contains(raw, `"committed":true`) {
		t.Fatal(raw)
	}
	if code, _, raw := sourceCLI(t, root, `{"query":"unavailable"}`, "call", "utcp-metrics", "query", "--args", "-", "--run", "utcp-run", "--id", "gap"); code == 0 || !strings.Contains(raw, `"committed":true`) {
		t.Fatal("native failure receipt missing", raw)
	}
	cliFinish := time.Now()
	db, err := store.OpenReadOnly(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	run, err := ledger.New(db, nil).Load(t.Context(), "utcp-run")
	if closeErr := db.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || len(run.Entries) != 2 {
		t.Fatalf("durable receipts missing: %+v %v", run, err)
	}
	wantFailureGap := failureReason + " " + nativeFailureImpact
	for i, want := range []struct{ id, status, gap string }{{"native", "collected", ""}, {"gap", "failed", wantFailureGap}} {
		entry := run.Entries[i]
		if entry.ID != want.id || entry.RunID != "utcp-run" || entry.Kind != ledger.KindEvidence {
			t.Fatalf("wrong persisted receipt: %+v", entry)
		}
		payload, err := ledger.DecodePayload(entry.Kind, entry.Payload)
		if err != nil {
			t.Fatal(err)
		}
		evidence := payload.(ledger.Evidence)
		if !strings.HasSuffix(evidence.Fact, "; "+want.status+" from utcp-metrics/query") || evidence.Source != references["utcp-metrics"] || evidence.Gap != want.gap || evidence.ObservedAt == nil || evidence.ObservedAt.IsZero() || evidence.ObservedAt.Location() != time.UTC || evidence.ObservedAt.Before(cliStart) || evidence.ObservedAt.After(cliFinish) {
			t.Fatalf("persisted receipt lost status, provenance or failure impact: %s %+v", want.id, evidence)
		}
	}
	var out, stderr bytes.Buffer
	if code := RunWithInput([]string{"report", "utcp-run", "--repo", root}, nil, &out, &stderr); code != 0 {
		t.Fatal("native report gap missing", code, out.String(), stderr.String())
	}
	var reportEnvelope struct {
		OK   bool         `json:"ok"`
		Data reportOutput `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &reportEnvelope); err != nil {
		t.Fatal(err)
	}
	document := reportEnvelope.Data
	if !reportEnvelope.OK || document.RunID != "utcp-run" || document.ThroughSeq != run.ThroughSeq || !slices.Contains(document.Gaps, "gap: "+wantFailureGap) || !strings.Contains(document.Markdown, wantFailureGap) {
		t.Fatalf("report lost persisted failure reason and impact: %+v", reportEnvelope)
	}
}
