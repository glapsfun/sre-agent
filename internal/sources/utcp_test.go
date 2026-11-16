package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const utcpInputs = `{"type":"object","properties":{"id":{"type":"string"},"query":{"type":"string"},"limit":{"type":"number"},"active":{"type":"boolean"}},"required":["id"],"additionalProperties":false}`

func utcpManual(endpoint, inputs, extra string) string {
	return fmt.Sprintf(`{"utcp_version":"1.1.0","manual_version":"one","tools":[{"name":"read","description":"untrusted descriptive text","inputs":%s,"tool_call_template":{"call_template_type":"http","url":%q,"http_method":"GET"%s}}]}`, inputs, endpoint, extra)
}

func utcpInventory(t *testing.T, root, endpoint string) Source {
	t.Helper()
	src := Source{ID: "metrics", Name: "Metrics", Kind: "utcp-http", Endpoint: endpoint, AccessRef: "provider-reader-role", CredentialEnv: "METRICS_TOKEN", Tools: []Tool{}}
	raw, _ := json.Marshal(map[string]any{"sources": []Source{src}})
	if err := os.WriteFile(filepath.Join(root, "known.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return src
}
func utcpService() Service {
	return Service{LookupEnv: func(key string) (string, bool) {
		if key != "METRICS_TOKEN" {
			panic("unapproved environment lookup")
		}
		return "private-bearer-value", true
	}}
}
func utcpApproved(t *testing.T, root, endpoint string) Source {
	t.Helper()
	utcpInventory(t, root, endpoint)
	report, err := utcpService().Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: "known.json"})
	if err != nil || len(report.Candidates) != 1 || len(report.Candidates[0].Tools) != 1 {
		t.Fatalf("scout: %+v %v", report, err)
	}
	src := report.Candidates[0].Source
	if _, err := Approve(context.Background(), root, ApprovalRequest{Approved: true, Sources: []Source{src}}); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestUTCPScoutAndCall(t *testing.T) {
	root := t.TempDir()
	var manual string
	var fetched, invoked atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer private-bearer-value" || r.Header.Get("Cookie") != "" {
			t.Errorf("unsafe request: %s %v", r.Method, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manual" {
			fetched.Add(1)
			fmt.Fprint(w, manual)
			return
		}
		invoked.Add(1)
		if r.URL.EscapedPath() != "/api/a%2Fb" || r.URL.Query().Get("query") != "up{job=\"x\"}" || r.URL.Query().Get("limit") != "9007199254740993" || r.URL.Query().Get("active") != "true" {
			t.Errorf("wrong URL: %s", r.URL)
		}
		fmt.Fprint(w, `{"status":"success","data":[],"echo":"private-bearer-value","token":"remote-secret"}`)
	}))
	defer ts.Close()
	manual = utcpManual(ts.URL+"/api/{id}", utcpInputs, `,"body_field":"body","headers":{},"header_fields":[],"auth":{"auth_type":"api_key","api_key":"Bearer ${METRICS_TOKEN}","var_name":"Authorization","location":"header"}`)
	utcpInventory(t, root, ts.URL+"/manual")
	if _, err := utcpService().Scout(context.Background(), root, ScoutOptions{KnownFile: "known.json"}); err != ErrApprovalRequired {
		t.Fatalf("consent: %v", err)
	}
	if fetched.Load() != 0 {
		t.Fatal("scout fetched without consent")
	}
	src := utcpApproved(t, root, ts.URL+"/manual")
	if invoked.Load() != 0 || !src.Tools[0].ReadOnly {
		t.Fatal("scout invoked tool or not selectable")
	}
	before, _ := os.ReadFile(filepath.Join(root, RegistryFilename))
	result, err := utcpService().Call(context.Background(), root, CallOptions{SourceID: "metrics", Tool: "read", Arguments: json.RawMessage(`{"id":"a/b","query":"up{job=\"x\"}","limit":9007199254740993,"active":true}`)})
	if err != nil || invoked.Load() != 1 || !json.Valid(result.Data) {
		t.Fatalf("call: %+v %v", result, err)
	}
	if strings.Contains(string(result.Data), "private-bearer-value") || strings.Contains(string(result.Data), "remote-secret") || !strings.Contains(string(result.Data), "success") {
		t.Fatalf("redaction: %s", result.Data)
	}
	if result.Reference != ts.URL+"/manual" || result.At.IsZero() {
		t.Fatalf("provenance: %+v", result)
	}
	after, _ := os.ReadFile(filepath.Join(root, RegistryFilename))
	if string(before) != string(after) {
		t.Fatal("call changed registry")
	}
	files, _ := os.ReadDir(root)
	if len(files) != 2 {
		t.Fatal("call persisted files")
	}
}

func TestUTCPRejectsArgumentsBeforeNativeRequest(t *testing.T) {
	for _, args := range []string{`{}`, `{"id":1}`, `{"id":"ok","extra":true}`, `{"id":"ok","query":null}`, `{"id":"ok","query":{}}`, `{"id":"ok","query":[]}`, `{"id":".."}`, `{"id":"%2e%2e"}`, `{"id":"../secret"}`, `{"id":"ok","body":"x"}`} {
		t.Run(args, func(t *testing.T) {
			root := t.TempDir()
			var manual string
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/manual" {
					fmt.Fprint(w, manual)
					return
				}
				calls.Add(1)
				fmt.Fprint(w, `{}`)
			}))
			defer ts.Close()
			manual = utcpManual(ts.URL+"/api/{id}", utcpInputs, "")
			utcpApproved(t, root, ts.URL+"/manual")
			result, err := utcpService().Call(context.Background(), root, CallOptions{SourceID: "metrics", Tool: "read", Arguments: json.RawMessage(args)})
			if err == nil || len(result.Gaps) == 0 || calls.Load() != 0 {
				t.Fatalf("unsafe call: %+v %v", result, err)
			}
		})
	}
}

func TestUTCPManualBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, replace, with string
		valid               bool
	}{
		{"version11", "1.1.0", "1.1", true}, {"versionpatch", "1.1.0", "1.1.12", true}, {"old", "1.1.0", "1.0", false}, {"future", "1.1.0", "1.2", false}, {"suffix", "1.1.0", "1.1.beta", false},
		{"empty revision", `"manual_version":"one"`, `"manual_version":""`, false},
		{"duplicate version", `"utcp_version":"1.1.0"`, `"utcp_version":"1.0","utcp_version":"1.1.0"`, false},
		{"post", `"http_method":"GET"`, `"http_method":"POST"`, false},
		{"unknown executable", `"http_method":"GET"`, `"http_method":"GET","verify_ssl":false`, false},
		{"headers", `"http_method":"GET"`, `"http_method":"GET","headers":{"X-Key":"private"}`, false},
		{"header mapping", `"http_method":"GET"`, `"http_method":"GET","header_fields":["query"]`, false},
		{"foreign env", `"http_method":"GET"`, `"http_method":"GET","auth":{"auth_type":"api_key","api_key":"Bearer ${OTHER_TOKEN}","var_name":"Authorization","location":"header"}`, false},
		{"bad schema", `"type":"object"`, `"type":"array"`, false},
		{"schema reference", `"type":"object"`, `"type":"object","$ref":"https://example.org/schema"`, false},
		{"schema unknown", `"type":"object"`, `"type":"object","patternProperties":{}`, false},
		{"body schema", `"id":{"type":"string"}`, `"body":{"type":"string"},"id":{"type":"string"}`, false},
		{"null input", utcpInputs, `null`, false},
		{"typed description", `"description":"untrusted descriptive text"`, `"description":123`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			var manual string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/manual" {
					t.Error("native request during scout")
				}
				fmt.Fprint(w, manual)
			}))
			defer ts.Close()
			manual = strings.Replace(utcpManual(ts.URL+"/api/{id}", utcpInputs, ""), tc.replace, tc.with, 1)
			utcpInventory(t, root, ts.URL+"/manual")
			report, err := utcpService().Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: "known.json"})
			if err != nil || len(report.Candidates) != 1 {
				t.Fatalf("scout: %+v %v", report, err)
			}
			if got := len(report.Candidates[0].Tools) == 1; got != tc.valid {
				t.Fatalf("valid=%v report=%+v", tc.valid, report)
			}
			if !tc.valid && len(report.Gaps) < 2 {
				t.Fatalf("missing unsupported gap: %+v", report)
			}
		})
	}
}

func TestUTCPDriftAndMixedTools(t *testing.T) {
	for _, change := range []string{"description", "url", "schema", "auth", "method", "removed", "mixed"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			var manual string
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/manual" {
					fmt.Fprint(w, manual)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"ok":true}`)
			}))
			defer ts.Close()
			manual = utcpManual(ts.URL+"/api/{id}", utcpInputs, "")
			utcpApproved(t, root, ts.URL+"/manual")
			switch change {
			case "description":
				manual = strings.Replace(manual, "untrusted descriptive text", "changed descriptive text", 1)
			case "url":
				manual = strings.Replace(manual, "/api/", "/other/", 1)
			case "schema":
				manual = strings.Replace(manual, `"type":"string"`, `"type":"string","minLength":2`, 1)
			case "auth":
				manual = strings.Replace(manual, `"http_method":"GET"`, `"http_method":"GET","auth":{"auth_type":"api_key","api_key":"Bearer ${METRICS_TOKEN}","var_name":"Authorization","location":"header"}`, 1)
			case "method":
				manual = strings.Replace(manual, `"GET"`, `"POST"`, 1)
			case "removed":
				manual = `{"utcp_version":"1.1","manual_version":"one","tools":[]}`
			case "mixed":
				manual = strings.TrimSuffix(manual, "]}") + `,{"name":"write","tool_call_template":{"call_template_type":"cli","command":"rm"}}]}`
			}
			result, err := utcpService().Call(context.Background(), root, CallOptions{SourceID: "metrics", Tool: "read", Arguments: json.RawMessage(`{"id":"ok"}`)})
			if change == "description" || change == "mixed" {
				if err != nil || calls.Load() != 1 {
					t.Fatalf("independent valid tool blocked: %+v %v", result, err)
				}
			} else if err == nil || calls.Load() != 0 || len(result.Gaps) == 0 {
				t.Fatalf("drift accepted: %+v %v", result, err)
			}
			if change == "mixed" {
				report, e := utcpService().Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: "known.json"})
				if e != nil || len(report.Candidates[0].Tools) != 1 || len(report.Gaps) != 2 {
					t.Fatalf("mixed: %+v %v", report, e)
				}
			}
		})
	}
}

func TestUTCPOriginAndPathRestrictions(t *testing.T) {
	for _, path := range []string{"http://example.com/api", "https://example.com/api", "http://127.0.0.1:1/api", "//example.com/api", "http://{host}/api", "/../secret", "/%2e%2e/secret", "/%252e%252e/secret", "/api?query=secret", "/api#fragment", "/api/password=secret", "/api/%0a", "/api/{bad", "/api/{body}"} {
		t.Run(path, func(t *testing.T) {
			root := t.TempDir()
			var manual string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, manual) }))
			defer ts.Close()
			endpoint := path
			if strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") {
				endpoint = ts.URL + path
			}
			manual = utcpManual(endpoint, utcpInputs, "")
			utcpInventory(t, root, ts.URL+"/manual")
			report, err := utcpService().Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: "known.json"})
			if err != nil || len(report.Candidates) != 1 || len(report.Candidates[0].Tools) != 0 {
				t.Fatalf("unsafe template accepted: %+v %v", report, err)
			}
		})
	}
}

func TestUTCPTransportFailuresAndText(t *testing.T) {
	for _, mode := range []string{"redirect", "denied", "length", "chunked", "invalid JSON", "invalid UTF8", "duplicate JSON", "empty", "null", "timeout", "text", "empty object", "empty array"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			var manual string
			var calls, redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
			defer target.Close()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/manual" {
					fmt.Fprint(w, manual)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				switch mode {
				case "redirect":
					http.Redirect(w, r, target.URL+"?secret=private-filter", http.StatusTemporaryRedirect)
				case "denied":
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, "private-filter private-bearer-value")
				case "length":
					w.Header().Set("Content-Length", "1048577")
					w.WriteHeader(200)
				case "chunked":
					w.(http.Flusher).Flush()
					io.WriteString(w, strings.Repeat("x", 1048577))
				case "invalid JSON":
					fmt.Fprint(w, `{"secret":"private-filter"`)
				case "invalid UTF8":
					w.Write([]byte{0xff})
				case "duplicate JSON":
					fmt.Fprint(w, `{"x":1,"x":2}`)
				case "empty":
					w.WriteHeader(204)
				case "null":
					fmt.Fprint(w, "null")
				case "timeout":
					<-r.Context().Done()
				case "text":
					w.Header().Set("Content-Type", "text/plain")
					fmt.Fprint(w, "available private-bearer-value token=server-secret")
				case "empty object":
					fmt.Fprint(w, `{}`)
				case "empty array":
					fmt.Fprint(w, `[]`)
				}
			}))
			defer ts.Close()
			manual = utcpManual(ts.URL+"/api/{id}", utcpInputs, "")
			utcpApproved(t, root, ts.URL+"/manual")
			service := utcpService()
			jar, _ := cookiejar.New(nil)
			endpoint, _ := url.Parse(ts.URL)
			jar.SetCookies(endpoint, []*http.Cookie{{Name: "session", Value: "sensitive-cookie"}})
			service.HTTPClient = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
			opts := CallOptions{SourceID: "metrics", Tool: "read", Arguments: json.RawMessage(`{"id":"ok","query":"private-filter"}`)}
			if mode == "timeout" {
				opts.Timeout = 30 * time.Millisecond
			}
			result, err := service.Call(context.Background(), root, opts)
			if calls.Load() != 1 || redirected.Load() != 0 || service.HTTPClient.Jar != jar {
				t.Fatal("retry, redirect, or client mutation")
			}
			success := mode == "text" || mode == "empty object" || mode == "empty array"
			if success {
				if err != nil || !json.Valid(result.Data) {
					t.Fatalf("response: %+v %v", result, err)
				}
			} else if err == nil || len(result.Gaps) == 0 || len(result.Data) != 0 {
				t.Fatalf("failure hidden: %+v %v", result, err)
			}
			if mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout identity: %v", err)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "private-filter") || strings.Contains(string(encoded), "private-bearer-value") || strings.Contains(string(encoded), "server-secret") {
				t.Fatalf("leak: %s", encoded)
			}
		})
	}
}

func TestUTCPManualResponseBoundsAndMalformed(t *testing.T) {
	for _, mode := range []string{"redirect", "denied", "oversize", "complex", "UTF8", "duplicate names", "missing tools", "null tools", "many tools", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			var manual string
			var hits atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				switch mode {
				case "redirect":
					http.Redirect(w, r, "/manual?secret=hidden", 302)
				case "denied":
					w.WriteHeader(401)
					fmt.Fprint(w, "secret=hidden")
				case "oversize":
					w.(http.Flusher).Flush()
					fmt.Fprint(w, strings.Repeat(" ", 1048577))
				case "complex":
					fmt.Fprint(w, `{"utcp_version":"1.1","manual_version":"one","tools":[],"extra":[`+strings.Repeat(`{},`, 22000)+`{}]}`)
				case "UTF8":
					w.Write([]byte{0xff})
				case "timeout":
					<-r.Context().Done()
				default:
					fmt.Fprint(w, manual)
				}
			}))
			defer ts.Close()
			switch mode {
			case "duplicate names":
				manual = utcpManual(ts.URL+"/api", `{"type":"object"}`, "")
				manual = strings.TrimSuffix(manual, "]}") + `,{"name":"read","tool_call_template":{}}]}`
			case "missing tools":
				manual = `{"utcp_version":"1.1","manual_version":"one"}`
			case "null tools":
				manual = `{"utcp_version":"1.1","manual_version":"one","tools":null}`
			case "many tools":
				manual = `{"utcp_version":"1.1","manual_version":"one","tools":[` + strings.Repeat(`{"name":"read"},`, 100) + `{"name":"last"}]}`
			}
			utcpInventory(t, root, ts.URL+"/manual")
			opts := ScoutOptions{Approved: true, KnownFile: "known.json"}
			if mode == "timeout" {
				opts.Timeout = 30 * time.Millisecond
			}
			report, err := utcpService().Scout(context.Background(), root, opts)
			if mode == "timeout" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(report.Candidates) != 1 || len(report.Candidates[0].Tools) != 0 || len(report.Gaps) == 0 || hits.Load() != 1 {
				t.Fatalf("invalid manual accepted: %+v %v", report, err)
			}
		})
	}
}

func TestUTCPSelectionMergeCredentialsAndApproval(t *testing.T) {
	root := t.TempDir()
	var manual string
	var lookups atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-bearer-value" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/manual" {
			fmt.Fprint(w, manual)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer ts.Close()
	manual = utcpManual(ts.URL+"/api", `{"type":"object"}`, "")
	src := utcpApproved(t, root, ts.URL+"/manual")
	raw, _ := json.Marshal(map[string]any{"sources": []Source{src}})
	selection, err := DecodeSelection(raw)
	if err != nil || len(selection) != 1 {
		t.Fatalf("selection: %v", err)
	}
	original := src
	src.AccessRef = ""
	if _, err := Approve(context.Background(), root, ApprovalRequest{Approved: true, Sources: []Source{src}}); err == nil {
		t.Fatal("missing provider role accepted")
	}
	src = original
	src.Tools = append([]Tool(nil), src.Tools...)
	src.Tools[0].ReadOnly = false
	if _, err := Approve(context.Background(), root, ApprovalRequest{Approved: true, Sources: []Source{src}}); err == nil {
		t.Fatal("non-readonly approval accepted")
	}
	other := Source{ID: "other", Name: "Other", Kind: "infrastructure", Reference: "cluster/namespace/service", Tools: []Tool{}}
	registry, err := Approve(context.Background(), root, ApprovalRequest{Approved: true, Sources: []Source{other}})
	if err != nil || len(registry.Sources) != 2 {
		t.Fatalf("merge: %+v %v", registry, err)
	}
	src = original
	src.CredentialEnv = ""
	raw, _ = json.Marshal(map[string]any{"sources": []Source{src}})
	os.WriteFile(filepath.Join(root, "known.json"), raw, 0600)
	service := Service{LookupEnv: func(key string) (string, bool) {
		lookups.Add(1)
		if key != "METRICS_TOKEN" {
			t.Errorf("unapproved key %q", key)
		}
		return "private-bearer-value", true
	}}
	report, err := service.Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: "known.json"})
	if err != nil || len(report.Candidates) != 1 || len(report.Candidates[0].Tools) != 1 {
		t.Fatalf("saved credential match: %+v %v", report, err)
	}
	lookups.Store(0)
	result, err := service.Call(context.Background(), root, CallOptions{SourceID: "metrics", Tool: "read"})
	if err != nil || len(result.Data) == 0 || lookups.Load() != 1 {
		t.Fatalf("credential lookup not captured once: %d %+v %v", lookups.Load(), result, err)
	}
}

func TestUTCPPreservesStaticEscapedPath(t *testing.T) {
	root := t.TempDir()
	var manual string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manual" {
			fmt.Fprint(w, manual)
			return
		}
		if r.URL.EscapedPath() != "/api/a%2Fb/ok" {
			t.Errorf("template path changed: %s", r.URL.EscapedPath())
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer ts.Close()
	manual = utcpManual(ts.URL+"/api/a%2Fb/{id}", utcpInputs, "")
	utcpApproved(t, root, ts.URL+"/manual")
	_, err := utcpService().Call(context.Background(), root, CallOptions{SourceID: "metrics", Tool: "read", Arguments: json.RawMessage(`{"id":"ok"}`)})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUTCPRejectsUnsupportedNumericConstraints(t *testing.T) {
	for _, constraint := range []string{`"maximum":9007199254740992`, `"minimum":9007199254740993`, `"multipleOf":0.1`, `"enum":[9007199254740993]`, `"const":9007199254740993`} {
		t.Run(constraint, func(t *testing.T) {
			root := t.TempDir()
			var manual string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, manual) }))
			defer ts.Close()
			manual = utcpManual(ts.URL+"/api", `{"type":"object","properties":{"value":{"type":"number",`+constraint+`}}}`, "")
			utcpInventory(t, root, ts.URL+"/manual")
			report, err := utcpService().Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: "known.json"})
			if err != nil || len(report.Candidates) != 1 || len(report.Candidates[0].Tools) != 0 {
				t.Fatalf("unsupported numeric constraint accepted: %+v %v", report, err)
			}
		})
	}
}

func TestUTCPRejectsAssembledTraversal(t *testing.T) {
	root := t.TempDir()
	var manual string
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manual" {
			fmt.Fprint(w, manual)
			return
		}
		calls.Add(1)
		fmt.Fprint(w, `{}`)
	}))
	defer ts.Close()
	manual = utcpManual(ts.URL+"/api/.{id}", utcpInputs, "")
	utcpApproved(t, root, ts.URL+"/manual")
	result, err := utcpService().Call(context.Background(), root, CallOptions{SourceID: "metrics", Tool: "read", Arguments: json.RawMessage(`{"id":""}`)})
	if err == nil || calls.Load() != 0 || len(result.Gaps) == 0 {
		t.Fatalf("assembled traversal allowed: %+v %v", result, err)
	}
}

func TestUTCPNumericTypesAndLexemes(t *testing.T) {
	for _, tc := range []struct {
		kind, number string
		valid        bool
	}{
		{"number", "9007199254740993", true}, {"integer", "9007199254740993", true}, {"integer", "18446744073709551615", true}, {"integer", "1.0e3", true}, {"number", "1.2300e-2", true}, {"integer", "9007199254740992.1", false}, {"number", "9007199254740992.1", false}, {"number", "1e1000000", false}, {"integer", "1.1", false}, {"string", "1", false},
	} {
		t.Run(tc.kind+tc.number, func(t *testing.T) {
			root := t.TempDir()
			var manual string
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/manual" {
					fmt.Fprint(w, manual)
					return
				}
				calls.Add(1)
				if r.URL.Query().Get("value") != tc.number {
					t.Errorf("number spelling changed: %s", r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{}`)
			}))
			defer ts.Close()
			manual = utcpManual(ts.URL+"/api", `{"type":"object","properties":{"value":{"type":"`+tc.kind+`"}}}`, "")
			utcpApproved(t, root, ts.URL+"/manual")
			result, err := utcpService().Call(context.Background(), root, CallOptions{SourceID: "metrics", Tool: "read", Arguments: json.RawMessage(`{"value":` + tc.number + `}`)})
			if tc.valid {
				if err != nil || calls.Load() != 1 {
					t.Fatalf("valid number: %+v %v", result, err)
				}
			} else if err == nil || calls.Load() != 0 {
				t.Fatalf("invalid number accepted: %+v %v", result, err)
			}
		})
	}
}

func TestUTCPRejectsMalformedTypedFields(t *testing.T) {
	for _, field := range []string{`"tags":[null]`, `"description":null`, `"average_response_size":1.2`, `"inputs":{"type":"object","properties":null}`, `"inputs":{"type":"object","required":[null]}`, `"inputs":{"type":"object","properties":{"x":{"type":"string","minLength":null}}}`} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			var manual string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, manual) }))
			defer ts.Close()
			manual = fmt.Sprintf(`{"utcp_version":"1.1","manual_version":"one","tools":[{"name":"read",%s,"tool_call_template":{"call_template_type":"http","url":%q,"http_method":"GET"}}]}`, field, ts.URL+"/api")
			utcpInventory(t, root, ts.URL+"/manual")
			report, err := utcpService().Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: "known.json"})
			if err != nil || len(report.Candidates) != 1 || len(report.Candidates[0].Tools) != 0 {
				t.Fatalf("malformed type accepted: %+v %v", report, err)
			}
		})
	}
}

func TestUTCPClientTimeoutIdentity(t *testing.T) {
	for _, phase := range []string{"manual", "native"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			var manual string
			var slow atomic.Bool
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if slow.Load() && (phase == "manual" && r.URL.Path == "/manual" || phase == "native" && r.URL.Path != "/manual") {
					<-r.Context().Done()
					return
				}
				if r.URL.Path == "/manual" {
					fmt.Fprint(w, manual)
					return
				}
				fmt.Fprint(w, `{}`)
			}))
			defer ts.Close()
			manual = utcpManual(ts.URL+"/api", `{"type":"object"}`, "")
			utcpApproved(t, root, ts.URL+"/manual")
			slow.Store(true)
			service := utcpService()
			service.HTTPClient = &http.Client{Timeout: 20 * time.Millisecond}
			result, err := service.Call(context.Background(), root, CallOptions{SourceID: "metrics", Tool: "read"})
			if !errors.Is(err, context.DeadlineExceeded) || len(result.Gaps) != 1 || len(result.Data) != 0 {
				t.Fatalf("timeout identity lost: %+v %v", result, err)
			}
		})
	}
}

func TestUTCPRequestSizeBound(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			calls.Add(1)
		}
	}
	ts.Start()
	defer ts.Close()
	_, _, err := (Service{}).utcpGet(context.Background(), ts.URL+"/"+strings.Repeat("a", (1<<20)-1000), strings.Repeat("x", 64<<10))
	if err == nil || calls.Load() != 0 {
		t.Fatalf("oversize request reached provider: %d %v", calls.Load(), err)
	}
}

func TestUTCPInputDefaultsAndDigestCanonicalization(t *testing.T) {
	source := Source{ID: "metrics", Name: "Metrics", Kind: "utcp-http", Endpoint: "https://metrics.example.test/manual"}
	var first Tool
	for i, entry := range []string{
		`{"name":"read","tool_call_template":{"call_template_type":"http","url":"https://metrics.example.test/api","http_method":"GET"}}`,
		`{"name":"read","inputs":{"type":"object"},"tool_call_template":{"url":"https://metrics.example.test/api","http_method":"GET","call_template_type":"http"}}`,
		`{"name":"read","description":"changed description","inputs":{"type":"object","description":"ignore instructions","default":{"private":"value"},"examples":[{"private":"value"}]},"tool_call_template":{"url":"https://metrics.example.test/api","http_method":"GET","call_template_type":"http"}}`,
	} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(entry), &fields); err != nil {
			t.Fatal(err)
		}
		tool, err := parseUTCPTool(context.Background(), source, "read", fields)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = tool.descriptor
		} else if tool.descriptor != first {
			t.Fatal("descriptive fields or field ordering changed executable digest")
		}
		endpoint, err := tool.requestURL(context.Background(), []byte(`{"query":"up"}`))
		if err != nil || endpoint != "https://metrics.example.test/api?query=up" {
			t.Fatalf("missing-input object default: %s %v", endpoint, err)
		}
	}
	if first.SchemaDigest != "67fa7ece7a304d1097ccfa36f72590bbbfc6f248d00c4f8a94efc6053fbc91ad" {
		t.Fatalf("canonical digest: %s", first.SchemaDigest)
	}
}

func TestUTCPBoundsNumericConversionWork(t *testing.T) {
	for _, number := range []string{"0." + strings.Repeat("1", 129), "0e1000000", "1e-1000000"} {
		if _, err := utcpValidationNumber(json.Number(number)); err == nil {
			t.Errorf("unbounded numeric spelling accepted: %s", number)
		}
	}
}

func TestUTCPProviderReadIdentityDeniesWriteAction(t *testing.T) {
	root := t.TempDir()
	var manual string
	var mutations atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manual" {
			fmt.Fprint(w, manual)
			return
		}
		if r.URL.Query().Get("action") == "write" {
			if r.Header.Get("Authorization") != "Bearer fixture-admin" {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, "read-only identity cannot write")
				return
			}
			mutations.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer ts.Close()
	manual = utcpManual(ts.URL+"/api", `{"type":"object","properties":{"action":{"type":"string"}},"additionalProperties":false}`, "")
	utcpApproved(t, root, ts.URL+"/manual")
	result, err := utcpService().Call(context.Background(), root, CallOptions{SourceID: "metrics", Tool: "read", Arguments: json.RawMessage(`{"action":"write"}`)})
	if err == nil || len(result.Gaps) != 1 || len(result.Data) != 0 || mutations.Load() != 0 {
		t.Fatalf("provider denial was lost: %+v %v", result, err)
	}
}

func TestUTCPDoesNotRetryNativeRequestAfterProviderReceivesIt(t *testing.T) {
	for _, mode := range []string{"default", "client default", "injected transport"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			var manual string
			var nativeRequests atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/manual" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, manual)
					return
				}
				nativeRequests.Add(1)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
			}))
			defer ts.Close()
			manual = utcpManual(ts.URL+"/api", `{"type":"object"}`, "")
			utcpApproved(t, root, ts.URL+"/manual")
			service := utcpService()
			switch mode {
			case "client default":
				service.HTTPClient = &http.Client{}
			case "injected transport":
				transport := http.DefaultTransport.(*http.Transport).Clone()
				defer transport.CloseIdleConnections()
				service.HTTPClient = &http.Client{Transport: transport}
			}
			result, err := service.Call(context.Background(), root, CallOptions{SourceID: "metrics", Tool: "read"})
			if nativeRequests.Load() != 1 {
				t.Fatalf("native request attempts=%d, want exactly 1", nativeRequests.Load())
			}
			if err == nil || len(result.Gaps) != 1 || len(result.Data) != 0 {
				t.Fatalf("lost native failure: %+v %v", result, err)
			}
		})
	}
}

func TestUTCPOwnsHTTP1TransportWithoutChangingCaller(t *testing.T) {
	root := t.TempDir()
	var manual string
	var requests atomic.Int32
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.ProtoMajor != 1 {
			t.Errorf("retry-capable protocol negotiated: %s", r.Proto)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manual" {
			fmt.Fprint(w, manual)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()
	manual = utcpManual(ts.URL+"/api", `{"type":"object"}`, "")
	utcpInventory(t, root, ts.URL+"/manual")
	service := utcpService()
	service.HTTPClient = ts.Client()
	transport := service.HTTPClient.Transport.(*http.Transport)
	transport.ForceAttemptHTTP2 = true
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.Protocols.SetHTTP2(true)
	transport.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
	report, err := service.Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: "known.json"})
	if err != nil || len(report.Candidates) != 1 || len(report.Candidates[0].Tools) != 1 {
		t.Fatalf("scout: %+v %v", report, err)
	}
	if _, err = Approve(context.Background(), root, ApprovalRequest{Approved: true, Sources: []Source{report.Candidates[0].Source}}); err != nil {
		t.Fatal(err)
	}
	result, err := service.Call(context.Background(), root, CallOptions{SourceID: "metrics", Tool: "read"})
	if err != nil || requests.Load() != 3 || len(result.Data) == 0 {
		t.Fatalf("call: %+v %v requests=%d", result, err, requests.Load())
	}
	if service.HTTPClient.Transport != transport || transport.DisableKeepAlives || !transport.ForceAttemptHTTP2 || !transport.Protocols.HTTP2() || strings.Join(transport.TLSClientConfig.NextProtos, ",") != "h2,http/1.1" {
		t.Fatal("caller transport configuration changed")
	}
}
