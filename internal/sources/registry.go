package sources

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/safety"
)

const RegistryFilename = ".sre-sources.json"

var (
	ErrNoRegistry                  = errors.New("no approved source registry")
	ErrApprovalRequired            = errors.New("explicit approval and selected sources required")
	ErrCodeNetworkApprovalRequired = errors.New("explicit code network approval required")
	ErrBusy                        = errors.New("source registry busy: active or stale lock requires inspection")
)

type Tool struct {
	Name         string `json:"name"`
	SchemaDigest string `json:"schema_digest"`
	ReadOnly     bool   `json:"read_only"`
}

type CodeNetwork struct {
	HostPort  string `json:"host_port"`
	AccessRef string `json:"access_ref"`
}

type Source struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Kind          string       `json:"kind"`
	Endpoint      string       `json:"endpoint,omitempty"`
	CredentialEnv string       `json:"credential_env,omitempty"`
	AccessRef     string       `json:"access_ref,omitempty"`
	Reference     string       `json:"reference,omitempty"`
	Tools         []Tool       `json:"tools"`
	CodeNetwork   *CodeNetwork `json:"code_network,omitempty"`
}

type ApprovedSource struct {
	Source
	ApprovedAt time.Time `json:"approved_at"`
}

type Registry struct {
	Version int              `json:"version"`
	Sources []ApprovedSource `json:"sources"`
}

type ApprovalRequest struct {
	Approved            bool
	CodeNetworkApproved bool
	Sources             []Source
}

const (
	maxRegistryBytes = 1 << 20
	lockDirectory    = ".sre-sources.lock"
)

var (
	identifierPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	environmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	digestPattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	dnsLabelPattern    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	addressPartPattern = regexp.MustCompile(`^(?:[0-9]+|0[xX][0-9a-fA-F]+)$`)
	errInvalidSource   = errors.New("invalid or unsafe source descriptor")
	errInvalidRegistry = errors.New("invalid approved source registry")
	errStorage         = errors.New("source registry storage operation failed")
)

// ValidateSource checks nonsecret descriptors without granting approval or access.
func ValidateSource(source Source) error {
	if !identifierPattern.MatchString(source.ID) || strings.TrimSpace(source.Name) == "" || !safeText(source.Name, 128) || !safeText(source.Reference, 512) || !safeText(source.AccessRef, 512) || len(source.Tools) > 100 {
		return errInvalidSource
	}
	switch source.Kind {
	case "mcp-http", "utcp-http":
		if !validEndpoint(source.Endpoint) || (source.CredentialEnv != "" && !environmentPattern.MatchString(source.CredentialEnv)) {
			return errInvalidSource
		}
	case "mcp-stdio", "infrastructure":
		if source.Endpoint != "" || source.CredentialEnv != "" || len(source.Tools) != 0 {
			return errInvalidSource
		}
	default:
		return errInvalidSource
	}
	if grant := source.CodeNetwork; grant != nil {
		if (source.Kind != "mcp-http" && source.Kind != "utcp-http") || source.CredentialEnv == "" || strings.TrimSpace(grant.AccessRef) == "" || !safeText(grant.AccessRef, 512) {
			return errInvalidSource
		}
		destination, ok := codeNetworkDestination(source.Endpoint)
		if !ok || grant.HostPort != destination {
			return errInvalidSource
		}
	}
	seen := make(map[string]bool, len(source.Tools))
	for _, tool := range source.Tools {
		if !identifierPattern.MatchString(tool.Name) || !digestPattern.MatchString(tool.SchemaDigest) || seen[tool.Name] {
			return errInvalidSource
		}
		seen[tool.Name] = true
	}
	return nil
}

func safeText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0 && safety.NewRedactor().Redact(value) == value
}

func validEndpoint(value string) bool {
	if value == "" || !safeText(value, maxRegistryBytes) || strings.ContainsAny(value, "?#") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(u.Host, ":") {
		return false
	}
	// Inspect decoded paths too, so escaping cannot hide credential assignments.
	if !safeText(u.Path, maxRegistryBytes) {
		return false
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return false
		}
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || net.ParseIP(u.Hostname()).IsLoopback())
}

func codeNetworkDestination(endpoint string) (string, bool) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false
	}
	host := u.Hostname()
	authority := host
	if address, err := netip.ParseAddr(host); err == nil {
		if address.Zone() != "" {
			return "", false
		}
		if address.Is6() {
			authority = "[" + host + "]"
		}
		host = address.String()
	} else {
		if len(host) == 0 || len(host) > 253 {
			return "", false
		}
		addressLike := true
		for _, label := range strings.Split(host, ".") {
			if !dnsLabelPattern.MatchString(label) {
				return "", false
			}
			addressLike = addressLike && addressPartPattern.MatchString(label)
		}
		// Legacy numeric forms can resolve differently across URL consumers.
		if addressLike {
			return "", false
		}
		host = strings.ToLower(host)
	}
	port := "443"
	if u.Scheme == "http" {
		port = "80"
	}
	if explicit := u.Port(); explicit != "" {
		number, err := strconv.Atoi(explicit)
		if err != nil || number < 1 || number > 65535 {
			return "", false
		}
		port = strconv.Itoa(number)
		authority += ":" + explicit
	}
	if authority != u.Host {
		return "", false
	}
	return net.JoinHostPort(host, port), true
}

func validateApproval(source Source) error {
	if err := ValidateSource(source); err != nil {
		return err
	}
	if (source.Kind == "mcp-http" || source.Kind == "utcp-http") && strings.TrimSpace(source.AccessRef) == "" {
		return errors.New("HTTP source approval requires a read-only access reference")
	}
	for _, tool := range source.Tools {
		if !tool.ReadOnly {
			return errors.New("source approval requires read-only tools")
		}
	}
	return nil
}

func canonicalSource(source Source) Source {
	if source.CodeNetwork != nil {
		grant := *source.CodeNetwork
		source.CodeNetwork = &grant
	}
	tools := make([]Tool, len(source.Tools))
	copy(tools, source.Tools)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	source.Tools = tools
	return source
}

func Load(ctx context.Context, root string) (Registry, error) {
	if err := ctx.Err(); err != nil {
		return Registry{}, err
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return Registry{}, errStorage
	}
	defer dir.Close()
	return load(ctx, dir)
}

func load(ctx context.Context, dir *os.Root) (Registry, error) {
	if err := ctx.Err(); err != nil {
		return Registry{}, err
	}
	info, err := dir.Lstat(RegistryFilename)
	if errors.Is(err, os.ErrNotExist) {
		return Registry{}, ErrNoRegistry
	}
	if err != nil {
		return Registry{}, errStorage
	}
	if !info.Mode().IsRegular() || info.Size() > maxRegistryBytes {
		return Registry{}, errInvalidRegistry
	}
	file, err := dir.Open(RegistryFilename)
	if err != nil {
		return Registry{}, errStorage
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return Registry{}, errInvalidRegistry
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxRegistryBytes+1))
	if err != nil {
		return Registry{}, errStorage
	}
	if err := ctx.Err(); err != nil {
		return Registry{}, err
	}
	if len(raw) > maxRegistryBytes || !utf8.Valid(raw) {
		return Registry{}, errInvalidRegistry
	}
	var registry Registry
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registry); err != nil || registry.Version != 1 || len(registry.Sources) > 64 {
		return Registry{}, errInvalidRegistry
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Registry{}, errInvalidRegistry
	}
	if err := uniqueFields(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return Registry{}, errInvalidRegistry
	}
	seen := make(map[string]bool, len(registry.Sources))
	for i, approved := range registry.Sources {
		if seen[approved.ID] || approved.ApprovedAt.IsZero() || approved.ApprovedAt.Year() < 1 || approved.ApprovedAt.Year() > 9999 || validateApproval(approved.Source) != nil {
			return Registry{}, errInvalidRegistry
		}
		seen[approved.ID] = true
		registry.Sources[i].Source = canonicalSource(approved.Source)
	}
	sort.Slice(registry.Sources, func(i, j int) bool { return registry.Sources[i].ID < registry.Sources[j].ID })
	return registry, nil
}

// The standard decoder keeps the last duplicate field, which can conceal an
// unsafe descriptor or schema version behind a second, apparently valid value.
func uniqueFields(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return errInvalidRegistry
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			token, err := decoder.Token()
			key, ok := token.(string)
			key = strings.ToLower(key)
			if err != nil || !ok || seen[key] {
				return errInvalidRegistry
			}
			seen[key] = true
		}
		if err := uniqueFields(decoder); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return errInvalidRegistry
	}
	return nil
}

// ValidateApprovalRequest checks consent and selection without repository I/O.
func ValidateApprovalRequest(req ApprovalRequest) error {
	if !req.Approved || len(req.Sources) == 0 {
		return ErrApprovalRequired
	}
	hasCodeNetwork := false
	for _, source := range req.Sources {
		hasCodeNetwork = hasCodeNetwork || source.CodeNetwork != nil
	}
	if hasCodeNetwork && !req.CodeNetworkApproved {
		return ErrCodeNetworkApprovalRequired
	}
	if req.CodeNetworkApproved && !hasCodeNetwork {
		return errors.New("code network approval requires a selected code network grant")
	}
	if len(req.Sources) > 64 {
		return errInvalidRegistry
	}
	seen := make(map[string]bool, len(req.Sources))
	for _, source := range req.Sources {
		if err := validateApproval(source); err != nil {
			return err
		}
		if seen[source.ID] {
			return errInvalidSource
		}
		seen[source.ID] = true
	}
	return nil
}

func Approve(ctx context.Context, root string, req ApprovalRequest) (Registry, error) {
	if err := ctx.Err(); err != nil {
		return Registry{}, err
	}
	if err := ValidateApprovalRequest(req); err != nil {
		return Registry{}, err
	}
	selected := make(map[string]Source, len(req.Sources))
	for _, source := range req.Sources {
		selected[source.ID] = canonicalSource(source)
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return Registry{}, errStorage
	}
	defer dir.Close()
	if err := ctx.Err(); err != nil {
		return Registry{}, err
	}
	if err := dir.Mkdir(lockDirectory, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Registry{}, ErrBusy
		}
		return Registry{}, errStorage
	}
	defer dir.Remove(lockDirectory)
	registry, err := load(ctx, dir)
	if errors.Is(err, ErrNoRegistry) {
		registry = Registry{Version: 1, Sources: []ApprovedSource{}}
	} else if err != nil {
		return Registry{}, err
	}
	changed := false
	now := time.Now().UTC()
	for i, approved := range registry.Sources {
		if source, exists := selected[approved.ID]; exists {
			if !reflect.DeepEqual(source, approved.Source) {
				registry.Sources[i] = ApprovedSource{Source: source, ApprovedAt: now}
				changed = true
			}
			delete(selected, approved.ID)
		}
	}
	for _, source := range selected {
		registry.Sources = append(registry.Sources, ApprovedSource{Source: source, ApprovedAt: now})
		changed = true
	}
	if len(registry.Sources) > 64 {
		return Registry{}, errInvalidRegistry
	}
	if err := ctx.Err(); err != nil {
		return Registry{}, err
	}
	if !changed {
		return registry, nil
	}
	sort.Slice(registry.Sources, func(i, j int) bool { return registry.Sources[i].ID < registry.Sources[j].ID })
	raw, err := json.MarshalIndent(registry, "", "  ")
	if err != nil || len(raw)+1 > maxRegistryBytes {
		return Registry{}, errInvalidRegistry
	}
	if err := writeRegistry(ctx, dir, append(raw, '\n')); err != nil {
		return Registry{}, err
	}
	return registry, nil
}

func writeRegistry(ctx context.Context, dir *os.Root, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name := ".sre-sources-" + rand.Text() + ".tmp"
	file, err := dir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errStorage
	}
	defer dir.Remove(name)
	defer file.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		return errStorage
	}
	if err := file.Sync(); err != nil {
		return errStorage
	}
	if err := file.Close(); err != nil {
		return errStorage
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dir.Rename(name, RegistryFilename); err != nil {
		return errStorage
	}
	return nil
}
