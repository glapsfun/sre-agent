package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/discover"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func writeScoutFile(t *testing.T, root, name, data string) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func inventory(t *testing.T, root string, sources ...Source) string {
	t.Helper()
	b, e := json.Marshal(struct {
		Sources []Source `json:"sources"`
	}{sources})
	if e != nil {
		t.Fatal(e)
	}
	return writeScoutFile(t, root, "known.json", string(b))
}
func httpSource(id, url string) Source {
	return Source{ID: id, Name: id, Kind: "mcp-http", Endpoint: url}
}
func scoutServer(t *testing.T, auth string, methods *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		t.Error("scout invoked tool")
		return &mcp.CallToolResult{}, nil
	})
	server.AddTool(&mcp.Tool{Name: "write", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		t.Error("scout invoked tool")
		return &mcp.CallToolResult{}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*methods = append(*methods, r.Method)
		mu.Unlock()
		if r.Header.Get("Authorization") != auth {
			t.Errorf("unexpected auth %q", r.Header.Get("Authorization"))
			w.WriteHeader(401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}
func TestScoutConsentBeforeAllIO(t *testing.T) {
	s := Service{LookupEnv: func(string) (string, bool) { t.Fatal("env accessed"); return "", false }, Discover: func(context.Context, discover.Options) (discover.Report, error) {
		t.Fatal("probe accessed")
		return discover.Report{}, nil
	}}
	for range 2 {
		_, err := s.Scout(context.Background(), "/nonexistent", ScoutOptions{KnownFile: "/nonexistent", Context: "prod"})
		if !errors.Is(err, ErrApprovalRequired) {
			t.Fatalf("got %v", err)
		}
	}
}
func TestScoutKnownListsToolsAndKeepsFilesystem(t *testing.T) {
	root := t.TempDir()
	var methods []string
	var mu sync.Mutex
	ts := scoutServer(t, "Bearer secret", &methods, &mu)
	src := httpSource("known", ts.URL)
	src.CredentialEnv = "APP_TOKEN"
	file := inventory(t, root, src)
	before, _ := os.ReadDir(root)
	lookups := 0
	s := Service{LookupEnv: func(key string) (string, bool) {
		lookups++
		if key != "APP_TOKEN" {
			t.Fatal(key)
		}
		return "secret", true
	}}
	report, err := s.Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: file})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Candidates) != 1 || report.Candidates[0].Status != "ready" {
		t.Fatalf("%+v", report)
	}
	got := report.Candidates[0].Tools
	if len(got) != 2 || !got[0].ReadOnly || got[1].ReadOnly || got[0].SchemaDigest != "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0" {
		t.Fatalf("tools %+v", got)
	}
	if lookups != 1 {
		t.Fatal(lookups)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range methods {
		if m == "GET" {
			t.Fatal("standalone SSE")
		}
	}
	after, _ := os.ReadDir(root)
	if len(before) != len(after) {
		t.Fatal("filesystem changed")
	}
}
func TestScoutRejectsInvalidKnownBeforeContact(t *testing.T) {
	root := t.TempDir()
	for _, raw := range []string{`null`, `{"sources":[],"extra":1}`, `{"sources":[],"sources":[]}`, `{"sources":[{"id":"a","name":"a","kind":"mcp-http","endpoint":"https://host/?secret=x"}]}`} {
		file := writeScoutFile(t, root, "bad.json", raw)
		_, err := (Service{Discover: func(context.Context, discover.Options) (discover.Report, error) {
			t.Fatal("probe before validation")
			return discover.Report{}, nil
		}}).Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: file, Context: "prod"})
		if err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
func TestScoutConfigPartialAndNoCredentialExfil(t *testing.T) {
	root := t.TempDir()
	var methods []string
	var mu sync.Mutex
	ts := scoutServer(t, "", &methods, &mu)
	writeScoutFile(t, root, ".mcp.json", `{"mcpServers":{"web":{"url":"`+ts.URL+`","env":{"TOKEN":"$HOME"},"headers":{"Authorization":"secret"}},"stdio":{"command":"touch","args":["owned"]},"bad":{"url":"https://user:pass@example.com"}}}`)
	report, err := (Service{LookupEnv: func(string) (string, bool) { t.Fatal("untrusted env lookup"); return "", false }, Discover: func(context.Context, discover.Options) (discover.Report, error) {
		t.Fatal("implicit discovery")
		return discover.Report{}, nil
	}}).Scout(context.Background(), root, ScoutOptions{Approved: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Candidates) != 2 || len(report.Gaps) < 2 {
		t.Fatalf("%+v", report)
	}
	if _, err := os.Stat(filepath.Join(root, "owned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("launched stdio")
	}
	data, _ := json.Marshal(report)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "$HOME") {
		t.Fatal(string(data))
	}
}
func TestScoutOptionsAndMissingCredentials(t *testing.T) {
	root := t.TempDir()
	for _, timeout := range []time.Duration{-1, 61 * time.Second} {
		if _, err := (Service{}).Scout(context.Background(), root, ScoutOptions{Approved: true, Timeout: timeout}); err == nil {
			t.Fatal("invalid timeout accepted")
		}
	}
	src := httpSource("auth", "http://127.0.0.1:1")
	src.CredentialEnv = "TOKEN"
	file := inventory(t, root, src)
	report, err := (Service{LookupEnv: func(string) (string, bool) { return "", false }}).Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: file})
	if err != nil || len(report.Candidates) != 1 || report.Candidates[0].Status != "unavailable" {
		t.Fatalf("%+v %v", report, err)
	}
}

func TestScoutApprovedCredentialsAndKnownPreference(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			root := t.TempDir()
			var methods []string
			var mu sync.Mutex
			ts := scoutServer(t, "Bearer secret", &methods, &mu)
			approved := httpSource("approved", ts.URL)
			approved.CredentialEnv = "APPROVED_TOKEN"
			approved.AccessRef = "reader-policy"
			if _, err := Approve(context.Background(), root, ApprovalRequest{Approved: true, Sources: []Source{approved}}); err != nil {
				t.Fatal(err)
			}
			writeScoutFile(t, root, ".mcp.json", `{"mcpServers":{"repo":{"url":"`+ts.URL+`","credential_env":"STOLEN_TOKEN","env":{"APPROVED_TOKEN":"malicious"}}}}`)
			opts := ScoutOptions{Approved: true}
			key := "APPROVED_TOKEN"
			if known {
				src := httpSource("known", ts.URL)
				src.CredentialEnv = "KNOWN_TOKEN"
				opts.KnownFile = inventory(t, root, src)
				key = "KNOWN_TOKEN"
			}
			lookups := 0
			report, err := (Service{LookupEnv: func(name string) (string, bool) {
				lookups++
				if name != key {
					t.Errorf("lookup %s", name)
				}
				return "secret", true
			}}).Scout(context.Background(), root, opts)
			if err != nil || len(report.Candidates) != 1 || report.Candidates[0].Status != "ready" || lookups != 1 {
				t.Fatalf("%+v %v lookups=%d", report, err, lookups)
			}
			if known && report.Candidates[0].ID != "known" {
				t.Fatal("known not preferred")
			}
		})
	}
}
func TestScoutInfrastructureReferencesOnly(t *testing.T) {
	root := t.TempDir()
	calls := 0
	report, err := (Service{Discover: func(ctx context.Context, opts discover.Options) (discover.Report, error) {
		calls++
		if opts.Context != "prod" || !reflect.DeepEqual(opts.Only, []string{"env", "obs"}) {
			t.Fatalf("%+v", opts)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded discovery")
		}
		return discover.Report{Probes: map[string]discover.ProbeResult{"env": {Status: discover.OK}, "obs": {Status: discover.OK, Findings: discover.Findings{Obs: &discover.Observability{EndpointInventoryComplete: false, Endpoints: []discover.Endpoint{{Namespace: "observability", Service: "metrics", ReadinessURL: "http://127.0.0.1:1/ready", PortForward: []string{"touch", "owned"}}}}}}}}, nil
	}}).Scout(context.Background(), root, ScoutOptions{Approved: true, Context: "prod"})
	if err != nil || calls != 1 || len(report.Candidates) != 1 || len(report.Gaps) == 0 {
		t.Fatalf("%+v %v", report, err)
	}
	src := report.Candidates[0]
	if src.Kind != "infrastructure" || src.Endpoint != "" || src.Reference != "prod/observability/metrics" || src.Status != "integration-needed" {
		t.Fatalf("%+v", src)
	}
}
func TestScoutMalformedConfigAndRegistryPreserveKnown(t *testing.T) {
	root := t.TempDir()
	writeScoutFile(t, root, ".mcp.json", `{"mcpServers":`)
	writeScoutFile(t, root, RegistryFilename, `{"bad":"secret"}`)
	file := inventory(t, root, Source{ID: "local", Name: "Local", Kind: "mcp-stdio"})
	report, err := (Service{}).Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: file})
	if err != nil || len(report.Candidates) != 1 || len(report.Gaps) < 3 {
		t.Fatalf("%+v %v", report, err)
	}
}
func TestScoutRejectsSymlinkAndOversizeConfig(t *testing.T) {
	for _, symlink := range []bool{true, false} {
		t.Run(fmt.Sprint(symlink), func(t *testing.T) {
			root := t.TempDir()
			if symlink {
				target := writeScoutFile(t, t.TempDir(), "config", `{"mcpServers":{"x":{"command":"ignored"}}}`)
				if err := os.Symlink(target, filepath.Join(root, ".mcp.json")); err != nil {
					t.Fatal(err)
				}
			} else {
				writeScoutFile(t, root, ".mcp.json", strings.Repeat(" ", (1<<20)+1))
			}
			report, err := (Service{}).Scout(context.Background(), root, ScoutOptions{Approved: true})
			if err != nil || len(report.Candidates) != 0 || len(report.Gaps) < 2 {
				t.Fatalf("%+v %v", report, err)
			}
		})
	}
}
func TestScoutCandidateLimitAndCancellation(t *testing.T) {
	root := t.TempDir()
	entries := map[string]any{}
	for i := range 65 {
		entries[fmt.Sprintf("x%d", i)] = map[string]any{"command": "ignored"}
	}
	raw, _ := json.Marshal(map[string]any{"mcpServers": entries})
	writeScoutFile(t, root, ".mcp.json", string(raw))
	report, err := (Service{}).Scout(context.Background(), root, ScoutOptions{Approved: true})
	if err != nil || len(report.Candidates) != 64 || len(report.Gaps) < 2 {
		t.Fatalf("%+v %v", report, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = (Service{}).Scout(ctx, root, ScoutOptions{Approved: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestScoutRejectsBlankExplicitContext(t *testing.T) {
	_, err := (Service{Discover: func(context.Context, discover.Options) (discover.Report, error) {
		t.Error("blank context triggered discovery")
		return discover.Report{}, nil
	}}).Scout(context.Background(), t.TempDir(), ScoutOptions{Approved: true, Context: "   "})
	if err == nil {
		t.Fatal("accepted blank explicit context")
	}
}
func TestScoutKnownInvalidTailMakesNoRequests(t *testing.T) {
	root := t.TempDir()
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer ts.Close()
	a := httpSource("good", ts.URL)
	bad := httpSource("bad", "http://remote.example/mcp")
	file := inventory(t, root, a, bad)
	_, err := (Service{}).Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: file})
	if err == nil || requests.Load() != 0 {
		t.Fatalf("err=%v requests=%d", err, requests.Load())
	}
}
func TestScoutPartialHTTPFailure(t *testing.T) {
	root := t.TempDir()
	var methods []string
	var mu sync.Mutex
	ts := scoutServer(t, "", &methods, &mu)
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "SECRET REMOTE DIAGNOSTIC", 403) }))
	defer failed.Close()
	file := inventory(t, root, httpSource("good", ts.URL), httpSource("bad", failed.URL))
	report, err := (Service{}).Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: file})
	if err != nil || len(report.Candidates) != 2 || report.Candidates[0].Status != "unavailable" || report.Candidates[1].Status != "ready" {
		t.Fatalf("%+v %v", report, err)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "SECRET") {
		t.Fatal(string(raw))
	}
}

func TestScoutMissingCredentialNeverContactsEndpoint(t *testing.T) {
	root := t.TempDir()
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(401) }))
	defer ts.Close()
	src := httpSource("auth", ts.URL)
	src.CredentialEnv = "TOKEN"
	file := inventory(t, root, src)
	report, err := (Service{LookupEnv: func(string) (string, bool) { return "", false }}).Scout(context.Background(), root, ScoutOptions{Approved: true, KnownFile: file})
	if err != nil || requests.Load() != 0 || len(report.Candidates) != 1 || report.Candidates[0].Status != "unavailable" {
		t.Fatalf("%+v %v requests=%d", report, err, requests.Load())
	}
}
func TestScoutStableIDsAcrossConfigurationLocations(t *testing.T) {
	root := t.TempDir()
	config := `{"mcpServers":{"same":{"command":"ignored"}}}`
	writeScoutFile(t, root, ".mcp.json", config)
	if err := os.Mkdir(filepath.Join(root, ".vscode"), 0700); err != nil {
		t.Fatal(err)
	}
	vscode := `{"servers":{"same":{"command":"ignored"}}}`
	writeScoutFile(t, root, ".vscode/mcp.json", vscode)
	first, err := (Service{}).Scout(context.Background(), root, ScoutOptions{Approved: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := (Service{}).Scout(context.Background(), root, ScoutOptions{Approved: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Candidates) != 2 || first.Candidates[0].ID == first.Candidates[1].ID || !reflect.DeepEqual(first, second) {
		t.Fatalf("%+v %+v", first, second)
	}
	for _, src := range first.Candidates {
		if ValidateSource(src.Source) != nil {
			t.Fatalf("invalid source %+v", src)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(root, ".mcp.json"))
	if string(raw) != config {
		t.Fatal("config changed")
	}
	raw, _ = os.ReadFile(filepath.Join(root, ".vscode/mcp.json"))
	if string(raw) != vscode {
		t.Fatal("vscode config changed")
	}
}
