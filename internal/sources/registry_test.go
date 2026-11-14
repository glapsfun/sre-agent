package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func candidate(id string) Source {
	return Source{ID: id, Name: "Metrics", Kind: "mcp-http", Endpoint: "https://metrics.example/mcp", CredentialEnv: "METRICS_TOKEN", AccessRef: "policy/metrics-reader", Tools: []Tool{{Name: "query", SchemaDigest: strings.Repeat("a", 64), ReadOnly: true}}}
}

func codeNetworkJSON(t *testing.T, source Source, grant string) []byte {
	t.Helper()
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(`{"sources":[{"code_network":` + grant + `,` + string(raw[1:]) + `]}`)
}

func TestCodeNetworkDescriptorValidation(t *testing.T) {
	valid := []struct{ endpoint, scope string }{
		{"https://MeTrIcS.Example/mcp", "metrics.example:443"},
		{"https://metrics.example:8443/mcp", "metrics.example:8443"},
		{"https://metrics.example:00443/mcp", "metrics.example:443"},
		{"https://192.0.2.1/mcp", "192.0.2.1:443"},
		{"https://[2001:0DB8:0:0:0:0:0:1]/mcp", "[2001:db8::1]:443"},
		{"https://[::ffff:192.0.2.1]:8443/mcp", "[::ffff:192.0.2.1]:8443"},
		{"http://LOCALHOST/mcp", "localhost:80"},
		{"http://127.0.0.1:8080/mcp", "127.0.0.1:8080"},
		{"http://[::1]/mcp", "[::1]:80"},
		{"https://" + strings.Repeat("a", 63) + ".example/mcp", strings.Repeat("a", 63) + ".example:443"},
	}
	for _, tt := range valid {
		t.Run(tt.endpoint, func(t *testing.T) {
			for _, kind := range []string{"mcp-http", "utcp-http"} {
				s := candidate("metrics")
				s.Kind, s.Endpoint = kind, tt.endpoint
				raw := codeNetworkJSON(t, s, `{"host_port":"`+tt.scope+`","access_ref":"policy/read-only and no anonymous writes"}`)
				if _, err := DecodeSelection(raw); err != nil {
					t.Fatalf("valid grant rejected: %v", err)
				}
			}
		})
	}
	invalid := []struct{ endpoint, scope string }{
		{"https://metrics.example/mcp", "other.example:443"},
		{"https://metrics.example/mcp", "metrics.example:80"},
		{"https://metrics.example/mcp", "METRICS.example:443"},
		{"https://metrics.example/mcp", "metrics.example:0443"},
		{"https://metrics.example/mcp", "metrics.example"},
		{"https://metrics.example/mcp", "*.example:443"},
		{"https://metrics.example/mcp", "https://metrics.example:443"},
		{"https://metrics.example/mcp", "user@metrics.example:443"},
		{"https://metrics.example/mcp", "metrics.example:443/path"},
		{"https://metrics.example/mcp", "metrics.example:443?x=y"},
		{"https://metrics.example./mcp", "metrics.example.:443"},
		{"https://métrics.example/mcp", "métrics.example:443"},
		{"https://bad_label.example/mcp", "bad_label.example:443"},
		{"https://-bad.example/mcp", "-bad.example:443"},
		{"https://bad-.example/mcp", "bad-.example:443"},
		{"https://bad..example/mcp", "bad..example:443"},
		{"https://*.example/mcp", "*.example:443"},
		{"https://127.1/mcp", "127.1:443"},
		{"https://2130706433/mcp", "2130706433:443"},
		{"https://0177.0.0.1/mcp", "0177.0.0.1:443"},
		{"https://0x7f.0.0.1/mcp", "0x7f.0.0.1:443"},
		{"https://0x7f000001/mcp", "0x7f000001:443"},
		{"https://999.0.0.1/mcp", "999.0.0.1:443"},
		{"https://[fe80::1%25eth0]/mcp", "[fe80::1%eth0]:443"},
		{"https://[::ffff:192.0.2.1]/mcp", "192.0.2.1:443"},
		{"https://[2001:db8::1]/mcp", "[2001:0db8::1]:443"},
		{"https://" + strings.Repeat("a", 64) + ".example/mcp", strings.Repeat("a", 64) + ".example:443"},
		{"https://" + strings.Repeat("abcd.", 50) + "abcd/mcp", strings.Repeat("abcd.", 50) + "abcd:443"},
	}
	for _, tt := range invalid {
		t.Run(tt.endpoint+"/"+tt.scope, func(t *testing.T) {
			s := candidate("metrics")
			s.Endpoint = tt.endpoint
			grant, _ := json.Marshal(map[string]string{"host_port": tt.scope, "access_ref": "policy/read-only"})
			if _, err := DecodeSelection(codeNetworkJSON(t, s, string(grant))); err == nil {
				t.Fatal("accepted unsafe destination")
			}
		})
	}
	// Ordinary source URL compatibility does not inherit the stricter grant grammar.
	for _, endpoint := range []string{"https://metrics.example./mcp", "https://métrics.example/mcp", "https://127.1/mcp"} {
		s := candidate("ordinary")
		s.Endpoint = endpoint
		if err := ValidateSource(s); err != nil {
			t.Fatalf("ordinary endpoint compatibility changed: %v", err)
		}
	}
}

func TestCodeNetworkStrictReaders(t *testing.T) {
	valid := `{"host_port":"metrics.example:443","access_ref":"policy/read-only"}`
	for _, grant := range []string{
		`{}`, `[]`, `true`, `"scope"`,
		`{"host_port":"metrics.example:443"}`,
		`{"host_port":"metrics.example:443","access_ref":" "}`,
		`{"host_port":"metrics.example:443","access_ref":"token=do-not-print"}`,
		`{"host_port":"metrics.example:443","access_ref":"line\nbreak"}`,
		`{"host_port":"metrics.example:443","access_ref":"` + strings.Repeat("a", 513) + `"}`,
		strings.Replace(valid, `"host_port":`, `"unknown":"do-not-print","host_port":`, 1),
		strings.Replace(valid, `"host_port":`, `"host_port":"evil.example:443","host_port":`, 1),
		strings.Replace(valid, `"access_ref":`, `"ACCESS_REF":"token=do-not-print","access_ref":`, 1),
	} {
		t.Run(grant, func(t *testing.T) {
			selection := codeNetworkJSON(t, candidate("metrics"), grant)
			if _, err := DecodeSelection(selection); err == nil || strings.Contains(err.Error(), "do-not-print") {
				t.Fatalf("unsafe selection accepted or leaked: %v", err)
			}
			registry := strings.Replace(string(selection), `{"sources":`, `{"version":1,"sources":`, 1)
			registry = strings.Replace(registry, `{"code_network":`, `{"approved_at":"2026-01-01T00:00:00Z","code_network":`, 1)
			root := t.TempDir()
			path := filepath.Join(root, RegistryFilename)
			if err := os.WriteFile(path, []byte(registry), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(t.Context(), root); err == nil || strings.Contains(err.Error(), "do-not-print") {
				t.Fatalf("unsafe registry accepted or leaked: %v", err)
			}
			if _, err := Approve(t.Context(), root, ApprovalRequest{Approved: true, Sources: []Source{candidate("new")}}); err == nil {
				t.Fatal("replaced unsafe registry")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != registry {
				t.Fatal("modified unsafe registry", err)
			}
		})
	}
}

func codeNetworkCandidate(id string) Source {
	s := candidate(id)
	s.CodeNetwork = &CodeNetwork{HostPort: "metrics.example:443", AccessRef: "policy/read-only and no anonymous writes"}
	return s
}

func TestCodeNetworkApprovalRequiresConsentBeforeIO(t *testing.T) {
	root := t.TempDir()
	for _, target := range []string{root, filepath.Join(root, "missing")} {
		_, err := Approve(t.Context(), target, ApprovalRequest{Approved: true, Sources: []Source{codeNetworkCandidate("metrics")}})
		if !errors.Is(err, ErrCodeNetworkApprovalRequired) {
			t.Fatalf("missing consent did not precede repository I/O: %v", err)
		}
	}
	requireEmpty(t, root)
}

func TestCodeNetworkCanonicalOwnership(t *testing.T) {
	s := codeNetworkCandidate("metrics")
	canonical := canonicalSource(s)
	s.CodeNetwork.HostPort = "other.example:443"
	s.CodeNetwork.AccessRef = "changed policy"
	if canonical.CodeNetwork.HostPort != "metrics.example:443" || canonical.CodeNetwork.AccessRef != "policy/read-only and no anonymous writes" {
		t.Fatal("canonical source aliases caller grant")
	}
}

func TestCodeNetworkRequiresAuthenticatedHTTPAndSafePolicy(t *testing.T) {
	for name, change := range map[string]func(*Source){
		"anonymous":           func(s *Source) { s.CredentialEnv = "" },
		"bad credential":      func(s *Source) { s.CredentialEnv = "9BAD" },
		"stdio":               func(s *Source) { s.Kind, s.Endpoint, s.CredentialEnv, s.Tools = "mcp-stdio", "", "", nil },
		"infrastructure":      func(s *Source) { s.Kind, s.Endpoint, s.CredentialEnv, s.Tools = "infrastructure", "", "", nil },
		"missing policy":      func(s *Source) { s.CodeNetwork.AccessRef = "" },
		"blank policy":        func(s *Source) { s.CodeNetwork.AccessRef = " \t" },
		"secret policy":       func(s *Source) { s.CodeNetwork.AccessRef = "password=do-not-print" },
		"control policy":      func(s *Source) { s.CodeNetwork.AccessRef = "policy\x7f" },
		"invalid UTF8 policy": func(s *Source) { s.CodeNetwork.AccessRef = string([]byte{255}) },
		"long policy":         func(s *Source) { s.CodeNetwork.AccessRef = strings.Repeat("p", 513) },
	} {
		t.Run(name, func(t *testing.T) {
			s := codeNetworkCandidate("metrics")
			change(&s)
			if err := ValidateSource(s); err == nil || strings.Contains(err.Error(), "do-not-print") {
				t.Fatalf("unsafe source accepted or leaked: %v", err)
			}
		})
	}
	s := codeNetworkCandidate("metrics")
	s.CodeNetwork.AccessRef = strings.Repeat("p", 512)
	if err := ValidateSource(s); err != nil {
		t.Fatalf("valid maximum policy rejected: %v", err)
	}
}

func TestCodeNetworkSharedPreflight(t *testing.T) {
	grant := codeNetworkCandidate("metrics")
	for _, tt := range []struct {
		name string
		req  ApprovalRequest
		want error
	}{
		{"ordinary consent absent", ApprovalRequest{CodeNetworkApproved: true, Sources: []Source{grant}}, ErrApprovalRequired},
		{"both absent", ApprovalRequest{Sources: []Source{grant}}, ErrApprovalRequired},
		{"special consent absent", ApprovalRequest{Approved: true, Sources: []Source{grant}}, ErrCodeNetworkApprovalRequired},
		{"ordinary selection", ApprovalRequest{Approved: true, Sources: []Source{candidate("ordinary")}}, nil},
		{"dual consent", ApprovalRequest{Approved: true, CodeNetworkApproved: true, Sources: []Source{grant}}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateApprovalRequest(tt.req); !errors.Is(err, tt.want) {
				t.Fatalf("preflight: %v, want %v", err, tt.want)
			}
			if tt.want != nil {
				root := t.TempDir()
				if err := os.Mkdir(filepath.Join(root, RegistryFilename), 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := Approve(t.Context(), root, tt.req); !errors.Is(err, tt.want) {
					t.Fatalf("unreadable registry took precedence over consent: %v", err)
				}
			}
		})
	}
	for name, selected := range map[string][]Source{
		"special flag without grant": {candidate("ordinary")},
		"empty selection":            {},
		"duplicate sources":          {grant, grant},
	} {
		t.Run(name, func(t *testing.T) {
			req := ApprovalRequest{Approved: true, CodeNetworkApproved: true, Sources: selected}
			if err := ValidateApprovalRequest(req); err == nil {
				t.Fatal("accepted invalid approval request")
			}
			root := t.TempDir()
			if _, err := Approve(t.Context(), root, req); err == nil {
				t.Fatal("saved invalid approval request")
			}
			requireEmpty(t, root)
		})
	}
	for _, change := range []func(*Source){
		func(s *Source) { s.AccessRef = "" },
		func(s *Source) { s.Tools[0].ReadOnly = false },
	} {
		s := codeNetworkCandidate("metrics")
		change(&s)
		if err := ValidateApprovalRequest(ApprovalRequest{Approved: true, CodeNetworkApproved: true, Sources: []Source{s}}); err == nil {
			t.Fatal("code network consent bypassed ordinary read-only checks")
		}
	}
}

func TestCodeNetworkApprovalLifecycle(t *testing.T) {
	root := t.TempDir()
	approve := func(s Source, network bool) Registry {
		t.Helper()
		got, err := Approve(t.Context(), root, ApprovalRequest{Approved: true, CodeNetworkApproved: network, Sources: []Source{s}})
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(t.Context(), root)
		if err != nil || !reflect.DeepEqual(got, loaded) {
			t.Fatalf("saved content differs: %+v %v", loaded, err)
		}
		return got
	}
	ordinary := approve(candidate("a"), false)
	if ordinary.Sources[0].CodeNetwork != nil {
		t.Fatal("ordinary approval inferred grant")
	}
	s := codeNetworkCandidate("a")
	first := approve(s, true)
	if first.Sources[0].CodeNetwork.HostPort != "metrics.example:443" || !first.Sources[0].ApprovedAt.After(ordinary.Sources[0].ApprovedAt) {
		t.Fatal("grant addition did not update approval")
	}
	second := approve(codeNetworkCandidate("z"), true)
	path := filepath.Join(root, RegistryFilename)
	oldTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Approve(t.Context(), root, ApprovalRequest{Approved: true, Sources: []Source{s}}); !errors.Is(err, ErrCodeNetworkApprovalRequired) {
		t.Fatalf("replay skipped consent: %v", err)
	}
	replay := approve(s, true)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(second, replay) || !info.ModTime().Equal(oldTime) || info.Mode().Perm() != 0600 {
		t.Fatal("replay modified private registry", err)
	}
	s.CodeNetwork.AccessRef = "policy/revised-read-only and no anonymous writes"
	updated := approve(s, true)
	if !updated.Sources[0].ApprovedAt.After(first.Sources[0].ApprovedAt) || updated.Sources[0].CodeNetwork.AccessRef != s.CodeNetwork.AccessRef || !reflect.DeepEqual(updated.Sources[1], second.Sources[1]) {
		t.Fatal("policy change did not replace only the selected grant")
	}
	s.CodeNetwork.AccessRef = "caller changed after approval"
	if updated.Sources[0].CodeNetwork.AccessRef != "policy/revised-read-only and no anonymous writes" {
		t.Fatal("returned approval aliases caller grant")
	}
	s = codeNetworkCandidate("a")
	s.Endpoint = "https://different.example/mcp"
	if _, err := Approve(t.Context(), root, ApprovalRequest{Approved: true, CodeNetworkApproved: true, Sources: []Source{s}}); err == nil {
		t.Fatal("changed destination reused old grant")
	}
	s.CodeNetwork.HostPort = "different.example:443"
	moved := approve(s, true)
	s.Endpoint = "https://different.example/new-path"
	if _, err := Approve(t.Context(), root, ApprovalRequest{Approved: true, Sources: []Source{s}}); !errors.Is(err, ErrCodeNetworkApprovalRequired) {
		t.Fatal("path change skipped special consent", err)
	}
	changedPath := approve(s, true)
	if !changedPath.Sources[0].ApprovedAt.After(moved.Sources[0].ApprovedAt) {
		t.Fatal("path change did not refresh approval")
	}
	removed := approve(candidate("a"), false)
	if removed.Sources[0].CodeNetwork != nil || !removed.Sources[0].ApprovedAt.After(changedPath.Sources[0].ApprovedAt) || !reflect.DeepEqual(removed.Sources[1], second.Sources[1]) {
		t.Fatal("ordinary replacement retained grant or changed unselected source")
	}
	approve(codeNetworkCandidate("a"), true)
	selected, err := DecodeSelection(codeNetworkJSON(t, candidate("a"), "null"))
	if err != nil || len(selected) != 1 || selected[0].CodeNetwork != nil {
		t.Fatal("null is not equivalent to omission", err)
	}
	removed = approve(selected[0], false)
	if removed.Sources[0].CodeNetwork != nil || !reflect.DeepEqual(removed.Sources[1], second.Sources[1]) {
		t.Fatal("null replacement retained grant or changed unselected source")
	}
}

func requireEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected no state, entries=%v err=%v", entries, err)
	}
}

func TestConsentAndMissingReadsCreateNoState(t *testing.T) {
	root := t.TempDir()
	if _, err := Load(context.Background(), root); !errors.Is(err, ErrNoRegistry) {
		t.Fatalf("missing registry: %v", err)
	}
	for _, req := range []ApprovalRequest{{Sources: []Source{candidate("metrics")}}, {Approved: true}} {
		if _, err := Approve(context.Background(), root, req); !errors.Is(err, ErrApprovalRequired) {
			t.Fatalf("missing consent/selection: %v", err)
		}
		requireEmpty(t, root)
	}
}

func TestApprovalMergesUpdatesSortsAndReplays(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	z := candidate("z")
	z.Tools = append(z.Tools, Tool{Name: "alpha", SchemaDigest: strings.Repeat("b", 64), ReadOnly: true})
	first, err := Approve(ctx, root, ApprovalRequest{Approved: true, Sources: []Source{z}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || len(first.Sources) != 1 || first.Sources[0].ApprovedAt.IsZero() || first.Sources[0].Tools[0].Name != "alpha" {
		t.Fatalf("invalid approval: %+v", first)
	}
	if z.Tools[0].Name != "query" {
		t.Fatal("mutated caller tools")
	}
	second, err := Approve(ctx, root, ApprovalRequest{Approved: true, Sources: []Source{candidate("a")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Sources) != 2 || second.Sources[0].ID != "a" || !reflect.DeepEqual(second.Sources[1], first.Sources[0]) {
		t.Fatalf("merge lost approval: %+v", second)
	}
	path := filepath.Join(root, RegistryFilename)
	oldTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := Approve(ctx, root, ApprovalRequest{Approved: true, Sources: []Source{z, candidate("a")}})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !info.ModTime().Equal(oldTime) || !reflect.DeepEqual(second, replay) {
		t.Fatal("replay changed registry")
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v", info.Mode())
	}
	z.Endpoint = "https://updated.example/mcp"
	updated, err := Approve(ctx, root, ApprovalRequest{Approved: true, Sources: []Source{z}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Sources[1].Endpoint != z.Endpoint || !updated.Sources[1].ApprovedAt.After(first.Sources[0].ApprovedAt) || !reflect.DeepEqual(updated.Sources[0], second.Sources[0]) {
		t.Fatalf("update: %+v", updated)
	}
	loaded, err := Load(ctx, root)
	if err != nil || !reflect.DeepEqual(loaded, updated) {
		t.Fatalf("load: %+v, %v", loaded, err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("temporary state left behind: %v", entries)
	}
}

func TestValidateSourceBoundaries(t *testing.T) {
	for _, endpoint := range []string{"https://metrics.example/mcp", "http://localhost:8080/mcp", "http://127.0.0.2/mcp", "http://[::1]:8080/mcp"} {
		s := candidate("valid")
		s.Endpoint, s.AccessRef, s.Tools[0].ReadOnly = endpoint, "", false
		if err := ValidateSource(s); err != nil {
			t.Errorf("valid scouting descriptor %q: %v", endpoint, err)
		}
	}
	for _, kind := range []string{"mcp-stdio", "infrastructure"} {
		if err := ValidateSource(Source{ID: "ref", Name: "Reference", Kind: kind, Reference: "integration needed"}); err != nil {
			t.Errorf("reference kind: %v", err)
		}
	}
	cases := map[string]func(*Source){
		"missing id":           func(s *Source) { s.ID = "" },
		"invalid id":           func(s *Source) { s.ID = "../bad" },
		"long id":              func(s *Source) { s.ID = strings.Repeat("a", 129) },
		"missing name":         func(s *Source) { s.Name = "" },
		"blank name":           func(s *Source) { s.Name = " " },
		"long name":            func(s *Source) { s.Name = strings.Repeat("a", 129) },
		"invalid utf8":         func(s *Source) { s.Name = string([]byte{255}) },
		"control name":         func(s *Source) { s.Name = "a\x7fb" },
		"secret name":          func(s *Source) { s.Name = "token=do-not-print" },
		"secret reference":     func(s *Source) { s.Reference = "password=do-not-print" },
		"secret access":        func(s *Source) { s.AccessRef = "api_key=do-not-print" },
		"long reference":       func(s *Source) { s.Reference = strings.Repeat("a", 513) },
		"long access":          func(s *Source) { s.AccessRef = strings.Repeat("a", 513) },
		"missing kind":         func(s *Source) { s.Kind = "" },
		"unknown kind":         func(s *Source) { s.Kind = "ssh" },
		"missing endpoint":     func(s *Source) { s.Endpoint = "" },
		"inline credential":    func(s *Source) { s.CredentialEnv = "TOKEN=do-not-print" },
		"invalid credential":   func(s *Source) { s.CredentialEnv = "9TOKEN" },
		"long credential":      func(s *Source) { s.CredentialEnv = strings.Repeat("A", 129) },
		"short digest":         func(s *Source) { s.Tools[0].SchemaDigest = "abc" },
		"uppercase digest":     func(s *Source) { s.Tools[0].SchemaDigest = strings.Repeat("A", 64) },
		"nonhex digest":        func(s *Source) { s.Tools[0].SchemaDigest = strings.Repeat("g", 64) },
		"bad tool":             func(s *Source) { s.Tools[0].Name = "query rows" },
		"empty tool":           func(s *Source) { s.Tools[0].Name = "" },
		"long tool":            func(s *Source) { s.Tools[0].Name = strings.Repeat("x", 129) },
		"duplicate tool":       func(s *Source) { s.Tools = append(s.Tools, s.Tools[0]) },
		"stdio endpoint":       func(s *Source) { s.Kind = "mcp-stdio" },
		"infrastructure tools": func(s *Source) { s.Kind, s.Endpoint, s.CredentialEnv = "infrastructure", "", "" },
		"stdio credential":     func(s *Source) { s.Kind, s.Endpoint, s.Tools = "mcp-stdio", "", nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := candidate("valid")
			change(&s)
			if err := ValidateSource(s); err == nil || strings.Contains(err.Error(), "do-not-print") {
				t.Fatalf("invalid source error: %v", err)
			}
		})
	}
	for _, endpoint := range []string{"http://example.com/mcp", "//example.com/mcp", "https:opaque", "ftp://example.com", "https://user:do-not-print@example.com", "https://example.com?token=do-not-print", "https://example.com?", "https://example.com#", "https://example.com#fragment", "https://", "https://example.com\n", "http://localhost.example.com", "https://example.com:bad", "https://example.com:99999", "https://example.com/%0A", "https://example.com/token%3Ddo-not-print"} {
		t.Run(endpoint, func(t *testing.T) {
			s := candidate("valid")
			s.Endpoint = endpoint
			if err := ValidateSource(s); err == nil || strings.Contains(err.Error(), "do-not-print") {
				t.Fatalf("invalid URL error: %v", err)
			}
		})
	}
}

func TestInvalidSelectionsCreateNoState(t *testing.T) {
	for _, kind := range []string{"write tool", "missing access", "duplicate source"} {
		t.Run(kind, func(t *testing.T) {
			root, s := t.TempDir(), candidate("metrics")
			sources := []Source{s}
			switch kind {
			case "write tool":
				sources[0].Tools[0].ReadOnly = false
			case "missing access":
				sources[0].AccessRef = ""
			case "duplicate source":
				sources = append(sources, s)
			}
			if _, err := Approve(context.Background(), root, ApprovalRequest{Approved: true, Sources: sources}); err == nil {
				t.Fatal("accepted invalid selection")
			}
			requireEmpty(t, root)
		})
	}
}

func TestMalformedEndpointAuthorities(t *testing.T) {
	for _, endpoint := range []string{"https://[not-an-ip]/mcp", "https://::1/mcp", "https://example.com:/mcp"} {
		t.Run(endpoint, func(t *testing.T) {
			s := candidate("metrics")
			s.Endpoint = endpoint
			if err := ValidateSource(s); err == nil {
				t.Fatal("accepted malformed endpoint authority")
			}
		})
	}
}

func registryBytes(t *testing.T, sources ...ApprovedSource) []byte {
	t.Helper()
	data, err := json.Marshal(Registry{Version: 1, Sources: sources})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLoadStrictAndRefusesOverwrite(t *testing.T) {
	approved := ApprovedSource{Source: candidate("metrics"), ApprovedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	valid := string(registryBytes(t, approved))
	cases := map[string]string{
		"duplicate version":          strings.Replace(valid, `"version":1`, `"version":2,"version":1`, 1),
		"duplicate descriptor field": strings.Replace(valid, `"name":"Metrics"`, `"name":"token=do-not-print","name":"Metrics"`, 1),
		"duplicate tool field":       strings.Replace(valid, `"read_only":true`, `"read_only":false,"read_only":true`, 1),
		"foreign schema":             strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"unknown field":              strings.Replace(valid, `"version":1`, `"version":1,"command":"do-not-print"`, 1),
		"unknown nested":             strings.Replace(valid, `"name":"Metrics"`, `"name":"Metrics","headers":{"Authorization":"do-not-print"}`, 1),
		"trailing object":            valid + `{}`,
		"trailing garbage":           valid + `garbage`,
		"array":                      `[]`, "null": `null`, "empty": `{}`, "malformed": `{"token":"do-not-print"`,
		"oversized":        valid + strings.Repeat(" ", 1<<20),
		"zero approval":    strings.Replace(valid, approved.ApprovedAt.Format(time.RFC3339), "0001-01-01T00:00:00Z", 1),
		"invalid approval": strings.Replace(valid, approved.ApprovedAt.Format(time.RFC3339), "do-not-print", 1),
		"missing approval": strings.Replace(valid, `,"approved_at":"2026-01-01T00:00:00Z"`, "", 1),
		"duplicate ids":    string(registryBytes(t, approved, approved)),
		"unsafe source":    strings.Replace(valid, `"name":"Metrics"`, `"name":"token=do-not-print"`, 1),
		"writable tool":    strings.Replace(valid, `"read_only":true`, `"read_only":false`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, RegistryFilename)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(context.Background(), root); err == nil || strings.Contains(err.Error(), "do-not-print") {
				t.Fatalf("load accepted invalid registry or leaked data: %v", err)
			}
			if _, err := Approve(context.Background(), root, ApprovalRequest{Approved: true, Sources: []Source{candidate("new")}}); err == nil || strings.Contains(err.Error(), "do-not-print") {
				t.Fatalf("approve invalid existing registry: %v", err)
			}
			after, _ := os.ReadFile(path)
			if string(after) != raw {
				t.Fatal("overwrote invalid registry")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 1 {
				t.Fatalf("temporary state left: %v", entries)
			}
		})
	}
}

func TestUnsafeFilesAndBusyLock(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "busy lock", "symlink lock"} {
		t.Run(kind, func(t *testing.T) {
			root, target := t.TempDir(), filepath.Join(t.TempDir(), "target")
			raw := registryBytes(t, ApprovedSource{Source: candidate("existing"), ApprovedAt: time.Now().UTC()})
			if err := os.WriteFile(target, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, filepath.Join(root, RegistryFilename))
			case "directory":
				err = os.Mkdir(filepath.Join(root, RegistryFilename), 0700)
			case "busy lock":
				err = os.Mkdir(filepath.Join(root, ".sre-sources.lock"), 0700)
			case "symlink lock":
				err = os.Symlink(target, filepath.Join(root, ".sre-sources.lock"))
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = Approve(context.Background(), root, ApprovalRequest{Approved: true, Sources: []Source{candidate("new")}})
			if err == nil {
				t.Fatal("accepted unsafe state")
			}
			if strings.Contains(kind, "lock") && !errors.Is(err, ErrBusy) {
				t.Fatalf("busy: %v", err)
			}
			if !strings.Contains(kind, "lock") {
				if _, err := Load(context.Background(), root); err == nil {
					t.Fatal("read unsafe file")
				}
			}
			after, _ := os.ReadFile(target)
			if !bytes.Equal(raw, after) {
				t.Fatal("changed symlink target")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 1 {
				t.Fatalf("changed existing state: %v", entries)
			}
		})
	}
}

func TestBounds(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	var selected []Source
	for i := range 64 {
		selected = append(selected, Source{ID: fmt.Sprintf("source-%02d", i), Name: "Reference", Kind: "infrastructure"})
	}
	if _, err := Approve(ctx, root, ApprovalRequest{Approved: true, Sources: selected}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, RegistryFilename))
	if _, err := Approve(ctx, root, ApprovalRequest{Approved: true, Sources: []Source{candidate("overflow")}}); err == nil {
		t.Fatal("accepted 65 sources")
	}
	after, _ := os.ReadFile(filepath.Join(root, RegistryFilename))
	if !bytes.Equal(before, after) {
		t.Fatal("overflow modified registry")
	}
	selected = append(selected, candidate("extra"))
	if _, err := Approve(ctx, t.TempDir(), ApprovalRequest{Approved: true, Sources: selected}); err == nil {
		t.Fatal("accepted 65 selected sources")
	}
	tooMany := make([]ApprovedSource, len(selected))
	for i, s := range selected {
		tooMany[i] = ApprovedSource{Source: s, ApprovedAt: time.Now().UTC()}
	}
	if err := os.WriteFile(filepath.Join(root, RegistryFilename), registryBytes(t, tooMany...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(ctx, root); err == nil {
		t.Fatal("loaded 65 sources")
	}
	s := candidate(strings.Repeat("a", 128))
	s.Name = strings.Repeat("n", 128)
	s.Reference = strings.Repeat("r", 512)
	s.AccessRef = strings.Repeat("a", 512)
	s.CredentialEnv = strings.Repeat("A", 128)
	s.Tools = nil
	for i := range 100 {
		s.Tools = append(s.Tools, Tool{Name: fmt.Sprintf("tool-%03d", i), SchemaDigest: strings.Repeat("0", 64), ReadOnly: true})
	}
	s.Tools[0].Name = strings.Repeat("t", 128)
	if err := ValidateSource(s); err != nil {
		t.Fatalf("maximum valid descriptor: %v", err)
	}
	s.Tools = append(s.Tools, Tool{Name: "overflow", SchemaDigest: strings.Repeat("0", 64), ReadOnly: true})
	if err := ValidateSource(s); err == nil {
		t.Fatal("accepted 101 tools")
	}
}

func TestCancellationAndInvalidRoot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	if _, err := Load(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled load: %v", err)
	}
	if _, err := Approve(ctx, root, ApprovalRequest{Approved: true, Sources: []Source{candidate("metrics")}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled approval: %v", err)
	}
	requireEmpty(t, root)
	missing := filepath.Join(root, "missing")
	if _, err := Approve(context.Background(), missing, ApprovalRequest{Approved: true, Sources: []Source{candidate("metrics")}}); err == nil {
		t.Fatal("created missing root")
	}
	requireEmpty(t, root)
}

func TestConcurrentApprovalsPreserveEverySuccessfulSelection(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			<-start
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, err := Approve(ctx, root, ApprovalRequest{Approved: true, Sources: []Source{candidate(fmt.Sprintf("source-%02d", i))}})
				if !errors.Is(err, ErrBusy) || time.Now().After(deadline) {
					results <- err
					return
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := Load(ctx, root)
	if err != nil || len(got.Sources) != 16 {
		t.Fatalf("lost concurrent updates: count=%d err=%v", len(got.Sources), err)
	}
}
