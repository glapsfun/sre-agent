package sources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/discover"
)

type Gap struct {
	SourceID string `json:"source_id,omitempty"`
	Reason   string `json:"reason"`
	Impact   string `json:"impact"`
}
type Candidate struct {
	Source
	Status string `json:"status"`
}
type ScoutReport struct {
	Candidates []Candidate `json:"candidates"`
	Gaps       []Gap       `json:"gaps"`
}
type ScoutOptions struct {
	Approved  bool
	KnownFile string
	Context   string
	Timeout   time.Duration
}
type Service struct {
	HTTPClient *http.Client
	LookupEnv  func(string) (string, bool)
	Discover   func(context.Context, discover.Options) (discover.Report, error)
	Now        func() time.Time
}

func (s Service) Scout(ctx context.Context, root string, opts ScoutOptions) (ScoutReport, error) {
	report := ScoutReport{Candidates: []Candidate{}, Gaps: []Gap{}}
	if !opts.Approved {
		return report, ErrApprovalRequired
	}
	if opts.Timeout < 0 || opts.Timeout > 60*time.Second || !safeText(opts.Context, 512) || opts.Context != "" && strings.TrimSpace(opts.Context) == "" {
		return report, errors.New("invalid source scout options")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	dir, err := os.OpenRoot(root)
	if root == "" || err != nil {
		if dir != nil {
			dir.Close()
		}
		return report, errors.New("source scout root must be an existing directory")
	}
	defer dir.Close()
	known := []Source{}
	if opts.KnownFile != "" {
		path := opts.KnownFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		raw, err := readScoutPath(path)
		if err != nil {
			return report, errors.New("invalid known source inventory")
		}
		var value struct {
			Sources []Source `json:"sources"`
		}
		if strictScoutJSON(raw, &value) != nil || len(value.Sources) > 64 || value.Sources == nil {
			return report, errors.New("invalid known source inventory")
		}
		ids := map[string]bool{}
		endpoints := map[string]bool{}
		for _, src := range value.Sources {
			if ValidateSource(src) != nil || ids[src.ID] || src.Endpoint != "" && endpoints[src.Endpoint] {
				return report, errors.New("invalid known source inventory")
			}
			ids[src.ID] = true
			endpoints[src.Endpoint] = true
		}
		known = value.Sources
	}
	gap := func(id, reason, impact string) {
		report.Gaps = append(report.Gaps, Gap{SourceID: id, Reason: reason, Impact: impact})
	}
	registry, err := Load(ctx, root)
	if err != nil && !errors.Is(err, ErrNoRegistry) {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		gap("", "approved registry unavailable", "Saved credential approvals cannot be used.")
	}
	credentials := map[string]string{}
	utcpCredentials := map[string]string{}
	if err == nil {
		for _, approved := range registry.Sources {
			if approved.Kind == "utcp-http" {
				if prior, ok := utcpCredentials[approved.Endpoint]; ok && prior != approved.CredentialEnv {
					utcpCredentials[approved.Endpoint] = ""
				} else if !ok {
					utcpCredentials[approved.Endpoint] = approved.CredentialEnv
				}
			}
			if approved.Kind == "mcp-http" {
				if prior, ok := credentials[approved.Endpoint]; ok && prior != approved.CredentialEnv {
					credentials[approved.Endpoint] = ""
				} else if !ok {
					credentials[approved.Endpoint] = approved.CredentialEnv
				}
			}
		}
	}
	for i := range known {
		if known[i].Kind == "utcp-http" && known[i].CredentialEnv == "" {
			known[i].CredentialEnv = utcpCredentials[known[i].Endpoint]
		}
	}
	all := append([]Source{}, known...)
	for _, config := range []struct{ path, key string }{{".mcp.json", "mcpServers"}, {".vscode/mcp.json", "servers"}} {
		raw, err := readScoutRoot(dir, config.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			gap("", "repository MCP configuration unavailable", "Some configured sources could not be inspected.")
			continue
		}
		var object map[string]json.RawMessage
		if strictScoutJSONLoose(raw, &object) != nil || object == nil {
			gap("", "repository MCP configuration invalid", "Some configured sources could not be inspected.")
			continue
		}
		var entries map[string]json.RawMessage
		if rawEntries, exists := object[config.key]; exists {
			if json.Unmarshal(rawEntries, &entries) != nil || entries == nil {
				gap("", "repository MCP configuration invalid", "Some configured sources could not be inspected.")
				continue
			}
		}
		keys := make([]string, 0, len(entries))
		for key := range entries {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			id := scoutID(config.path, key)
			if !identifierPattern.MatchString(key) || !safeText(key, 128) {
				gap(id, "configured source name is unsafe", "Source requires a safe identifier.")
				continue
			}
			var entry struct {
				URL     string `json:"url"`
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			if json.Unmarshal(entries[key], &entry) != nil {
				gap(id, "configured source is invalid", "Source cannot be contacted.")
				continue
			}
			src := Source{ID: id, Name: key, Tools: []Tool{}}
			switch {
			case entry.URL != "":
				src.Kind = "mcp-http"
				src.Endpoint = entry.URL
				src.CredentialEnv = credentials[entry.URL]
			case entry.Command != "" || entry.Type == "stdio":
				src.Kind = "mcp-stdio"
				src.Reference = config.path + ":" + key
			default:
				gap(id, "configured transport is unsupported", "Source needs an explicit integration.")
				continue
			}
			if ValidateSource(src) != nil {
				gap(id, "configured source endpoint is unsafe", "Source cannot be contacted.")
				continue
			}
			all = append(all, src)
		}
	}
	seenIDs := map[string]bool{}
	seenEndpoints := map[string]bool{}
	add := func(src Source) bool {
		if seenIDs[src.ID] {
			gap(src.ID, "duplicate source identifier skipped", "Source inventory is incomplete.")
			return false
		}
		if src.Endpoint != "" && seenEndpoints[src.Endpoint] {
			gap(src.ID, "duplicate source endpoint skipped", "The preferred inventory entry represents this endpoint.")
			return false
		}
		if len(report.Candidates) >= 64 {
			gap(src.ID, "source candidate limit reached", "Additional sources were not inspected.")
			return false
		}
		seenIDs[src.ID] = true
		if src.Endpoint != "" {
			seenEndpoints[src.Endpoint] = true
		}
		return true
	}
	for _, src := range all {
		if !add(src) {
			continue
		}
		src.Tools = []Tool{}
		candidate := Candidate{Source: src, Status: "integration-needed"}
		if src.Kind == "utcp-http" {
			credential, err := s.utcpCredential(src)
			if err != nil {
				candidate.Status = "unavailable"
				gap(src.ID, "source tool inventory unavailable", "This source cannot support the investigation.")
			} else {
				tools, partial, err := s.utcpManual(ctx, src, credential)
				if err != nil {
					candidate.Status = "unavailable"
					gap(src.ID, "source tool inventory unavailable", "This source cannot support the investigation.")
				} else {
					candidate.Status = "ready"
					for _, tool := range tools {
						candidate.Tools = append(candidate.Tools, tool.descriptor)
					}
					if partial {
						gap(src.ID, "some UTCP tools are unsupported", "Only supported HTTP GET tools are available for review.")
					}
				}
			}
		}
		if src.Kind == "mcp-http" {
			_, tools, cleanup, err := s.openSession(ctx, src)
			if err != nil {
				candidate.Status = "unavailable"
				gap(src.ID, "source tool inventory unavailable", "This source cannot support the investigation.")
			} else {
				candidate.Tools = tools
				candidate.Status = "ready"
				cleanup()
			}
		}
		report.Candidates = append(report.Candidates, candidate)
	}
	if opts.Context == "" {
		gap("", "infrastructure uninspected: no explicit context", "Cluster observability sources may be missing.")
	} else if ctx.Err() == nil {
		probe := s.Discover
		if probe == nil {
			probe = discover.Discover
		}
		found, err := probe(ctx, discover.Options{Context: opts.Context, Only: []string{"env", "obs"}, Timeout: opts.Timeout, SkipCluster: os.Getenv("SRE_SKIP_CLUSTER") == "true"})
		if err != nil {
			gap("", "infrastructure discovery unavailable", "Cluster observability inventory is incomplete.")
		}
		for _, name := range []string{"env", "obs"} {
			p, ok := found.Probes[name]
			if !ok || p.Status != discover.OK || len(p.Gaps) > 0 {
				gap("", "infrastructure "+name+" discovery incomplete", "Cluster observability inventory is incomplete.")
			}
		}
		obs := found.Probes["obs"].Findings.Obs
		if obs == nil || !obs.EndpointInventoryComplete {
			gap("", "observability endpoint inventory incomplete", "Undiscovered observability sources may affect the investigation.")
		}
		if obs != nil {
			for _, endpoint := range obs.Endpoints {
				if !identifierPattern.MatchString(endpoint.Namespace) || !identifierPattern.MatchString(endpoint.Service) || !safeText(endpoint.Namespace, 128) || !safeText(endpoint.Service, 128) {
					gap("", "unsafe infrastructure reference skipped", "Cluster observability inventory is incomplete.")
					continue
				}
				ref := opts.Context + "/" + endpoint.Namespace + "/" + endpoint.Service
				src := Source{ID: scoutID("infrastructure", ref), Name: endpoint.Service, Kind: "infrastructure", Reference: ref, Tools: []Tool{}}
				if ValidateSource(src) != nil {
					gap("", "unsafe infrastructure reference skipped", "Cluster observability inventory is incomplete.")
					continue
				}
				if add(src) {
					report.Candidates = append(report.Candidates, Candidate{Source: src, Status: "integration-needed"})
				}
			}
		}
	}
	sort.Slice(report.Candidates, func(i, j int) bool { return report.Candidates[i].ID < report.Candidates[j].ID })
	sort.Slice(report.Gaps, func(i, j int) bool {
		a, b := report.Gaps[i], report.Gaps[j]
		if a.SourceID != b.SourceID {
			return a.SourceID < b.SourceID
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		return a.Impact < b.Impact
	})
	if err := ctx.Err(); err != nil {
		return report, err
	}
	return report, nil
}

func scoutID(location, key string) string {
	sum := sha256.Sum256([]byte(location + "\x00" + key))
	return "source-" + hex.EncodeToString(sum[:])
}
func strictScoutJSON(raw []byte, value any) error {
	if err := strictScoutJSONLoose(raw, new(any)); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return errInvalidSource
	}
	return nil
}
func strictScoutJSONLoose(raw []byte, value any) error {
	if len(raw) > maxRegistryBytes || !utf8.Valid(raw) || strings.TrimSpace(string(raw)) == "null" {
		return errInvalidSource
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(value); err != nil {
		return errInvalidSource
	}
	if dec.Decode(new(any)) != io.EOF {
		return errInvalidSource
	}
	return uniqueFields(json.NewDecoder(bytes.NewReader(raw)))
}
func readScoutPath(path string) ([]byte, error) {
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return readScoutRoot(dir, filepath.Base(path))
}
func readScoutRoot(dir *os.Root, path string) ([]byte, error) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := 1; i < len(parts); i++ {
		info, err := dir.Lstat(filepath.Join(parts[:i]...))
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errInvalidSource
		}
	}
	info, err := dir.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxRegistryBytes {
		return nil, errInvalidSource
	}
	file, err := dir.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errInvalidSource
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxRegistryBytes+1))
	if err != nil || len(raw) > maxRegistryBytes {
		return nil, errInvalidSource
	}
	return raw, nil
}
