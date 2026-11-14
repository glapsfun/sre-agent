package sources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/google/jsonschema-go/jsonschema"
)

var errUTCP = errors.New("UTCP source unavailable or unsupported")
var utcpVersion = regexp.MustCompile(`^1\.1(?:\.[0-9]+)?$`)
var utcpEscapedPlaceholder = regexp.MustCompile(`(?i)%7b([A-Za-z0-9_.-]{1,128})%7d`)
var utcpPlaceholder = regexp.MustCompile(`\{([A-Za-z0-9_.-]{1,128})\}`)

type utcpTool struct {
	descriptor Tool
	schema     *jsonschema.Resolved
	endpoint   string
}

func (s Service) utcpCredential(source Source) (string, error) {
	if source.CredentialEnv == "" {
		return "", nil
	}
	lookup := s.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	value, ok := lookup(source.CredentialEnv)
	if !ok || value == "" || len(value) > 64<<10 || strings.ContainsAny(value, "\r\n") {
		return "", errUTCP
	}
	return value, nil
}

// utcpGet owns its client copy so injected cookie and redirect policies cannot
// widen the approved origin. The same captured credential serves both requests.
func (s Service) utcpGet(ctx context.Context, endpoint, credential string) ([]byte, string, error) {
	if len(endpoint) > maxRegistryBytes {
		return nil, "", errUTCP
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil || len(endpoint)+len(req.URL.Host)+len(credential)+1024 > maxRegistryBytes {
		return nil, "", errUTCP
	}
	req.Header.Set("Accept", "application/json, text/plain")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	client := http.Client{}
	if s.HTTPClient != nil {
		client = *s.HTTPClient
	}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errUTCP }
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if transport, ok := base.(*http.Transport); ok {
		// Fresh HTTP/1 connections prevent automatic replay after a provider
		// receives a GET and closes before responding. HTTP/2 can retry streams.
		owned := transport.Clone()
		owned.DisableKeepAlives = true
		owned.ForceAttemptHTTP2 = false
		owned.Protocols = new(http.Protocols)
		owned.Protocols.SetHTTP1(true)
		owned.TLSNextProto = nil
		if owned.TLSClientConfig != nil {
			owned.TLSClientConfig.NextProtos = []string{"http/1.1"}
		}
		client.Transport = owned
		defer owned.CloseIdleConnections()
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, "", utcpTransportError(ctx, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 || response.ContentLength > maxRegistryBytes {
		return nil, "", errUTCP
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxRegistryBytes+1))
	if err != nil || len(raw) > maxRegistryBytes || !utf8.Valid(raw) || ctx.Err() != nil {
		return nil, "", utcpTransportError(ctx, err)
	}
	return raw, response.Header.Get("Content-Type"), nil
}

func utcpJSON(ctx context.Context, raw []byte, value any) error {
	if len(raw) > maxRegistryBytes || !utf8.Valid(raw) || safety.DecodeJSON(ctx, string(raw), value) != nil {
		return errUTCP
	}
	if uniqueFields(json.NewDecoder(bytes.NewReader(raw))) != nil {
		return errUTCP
	}
	return nil
}

func (s Service) utcpManual(ctx context.Context, source Source, credential string) ([]utcpTool, bool, error) {
	if source.Kind != "utcp-http" || ValidateSource(source) != nil || !utcpSafePath(source.Endpoint) {
		return nil, false, errUTCP
	}
	raw, _, err := s.utcpGet(ctx, source.Endpoint, credential)
	if err != nil {
		return nil, false, err
	}
	var manual map[string]json.RawMessage
	if utcpJSON(ctx, raw, &manual) != nil || manual == nil {
		return nil, false, errUTCP
	}
	for k := range manual {
		if k != "utcp_version" && k != "manual_version" && k != "tools" {
			return nil, false, errUTCP
		}
	}
	var version, revision string
	var entries []json.RawMessage
	if json.Unmarshal(manual["utcp_version"], &version) != nil || !utcpVersion.MatchString(version) || json.Unmarshal(manual["manual_version"], &revision) != nil || strings.TrimSpace(revision) == "" || !safeText(revision, 128) || json.Unmarshal(manual["tools"], &entries) != nil || entries == nil || len(entries) > 100 {
		return nil, false, errUTCP
	}
	tools := []utcpTool{}
	seen := map[string]bool{}
	partial := false
	for _, entry := range entries {
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil || fields == nil {
			return nil, false, errUTCP
		}
		var name string
		if json.Unmarshal(fields["name"], &name) != nil || !identifierPattern.MatchString(name) || !safeText(name, 128) || seen[name] {
			return nil, false, errUTCP
		}
		seen[name] = true
		tool, err := parseUTCPTool(ctx, source, name, fields)
		if err != nil {
			partial = true
			continue
		}
		tools = append(tools, tool)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].descriptor.Name < tools[j].descriptor.Name })
	return tools, partial, nil
}

func parseUTCPTool(ctx context.Context, source Source, name string, fields map[string]json.RawMessage) (utcpTool, error) {
	for key, raw := range fields {
		switch key {
		case "name", "inputs", "tool_call_template":
		case "description":
			var value string
			if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
				return utcpTool{}, errUTCP
			}
		case "outputs":
			var value map[string]any
			if json.Unmarshal(raw, &value) != nil || value == nil {
				return utcpTool{}, errUTCP
			}
		case "tags":
			var value []any
			if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
				return utcpTool{}, errUTCP
			}
			for _, tag := range value {
				if _, ok := tag.(string); !ok {
					return utcpTool{}, errUTCP
				}
			}
		case "average_response_size":
			var value *int64
			if json.Unmarshal(raw, &value) != nil {
				return utcpTool{}, errUTCP
			}
		default:
			return utcpTool{}, errUTCP
		}
	}
	inputs := fields["inputs"]
	// The official Tool model defaults inputs to an unconstrained JsonSchema.
	// This adapter narrows that default to the object required by CallOptions.
	if len(inputs) == 0 {
		inputs = json.RawMessage(`{"type":"object"}`)
	}
	normalized, resolved, err := utcpSchema(ctx, inputs)
	if err != nil {
		return utcpTool{}, errUTCP
	}
	var template map[string]any
	if safety.DecodeJSON(ctx, string(fields["tool_call_template"]), &template) != nil || template == nil {
		return utcpTool{}, errUTCP
	}
	endpoint, err := utcpTemplate(source, template)
	if err != nil {
		return utcpTool{}, errUTCP
	}
	// Go's compact JSON sorts map keys recursively and preserves json.Number text.
	// Include every accepted template field, including explicit null/empty fields.
	canonical, err := json.Marshal(map[string]any{"inputs": normalized, "tool_call_template": template})
	if err != nil {
		return utcpTool{}, errUTCP
	}
	digest := sha256.Sum256(canonical)
	return utcpTool{descriptor: Tool{Name: name, SchemaDigest: hex.EncodeToString(digest[:]), ReadOnly: true}, schema: resolved, endpoint: endpoint}, nil
}

func utcpSchema(ctx context.Context, raw []byte) (map[string]any, *jsonschema.Resolved, error) {
	var schema map[string]any
	if safety.DecodeJSON(ctx, string(raw), &schema) != nil || schema == nil || schema["type"] != "object" {
		return nil, nil, errUTCP
	}
	normalized, err := utcpSchemaFields(schema, true)
	if err != nil {
		return nil, nil, errUTCP
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return nil, nil, errUTCP
	}
	var compiled jsonschema.Schema
	if json.Unmarshal(data, &compiled) != nil {
		return nil, nil, errUTCP
	}
	resolved, err := compiled.Resolve(nil)
	if err != nil {
		return nil, nil, errUTCP
	}
	return normalized, resolved, nil
}

func utcpSchemaFields(schema map[string]any, object bool) (map[string]any, error) {
	out := map[string]any{}
	kind, ok := schema["type"].(string)
	if !ok || object && kind != "object" || !object && kind != "string" && kind != "number" && kind != "integer" && kind != "boolean" {
		return nil, errUTCP
	}
	for key, value := range schema {
		switch key {
		case "type":
			out[key] = value
		case "title", "description":
			if _, ok := value.(string); !ok {
				return nil, errUTCP
			}
		case "default": // Defaults are never applied to an operator's arguments.
		case "examples":
			if _, ok := value.([]any); !ok {
				return nil, errUTCP
			}
		case "properties":
			props, ok := value.(map[string]any)
			if !object || !ok {
				return nil, errUTCP
			}
			normalized := map[string]any{}
			for name, raw := range props {
				property, ok := raw.(map[string]any)
				if !ok || name == "body" || !identifierPattern.MatchString(name) || !safeText(name, 128) {
					return nil, errUTCP
				}
				child, err := utcpSchemaFields(property, false)
				if err != nil {
					return nil, errUTCP
				}
				normalized[name] = child
			}
			out[key] = normalized
		case "required":
			values, ok := value.([]any)
			if !object || !ok {
				return nil, errUTCP
			}
			seen := map[string]bool{}
			for _, v := range values {
				name, ok := v.(string)
				if !ok || !identifierPattern.MatchString(name) || name == "body" || seen[name] {
					return nil, errUTCP
				}
				seen[name] = true
			}
			out[key] = value
		case "additionalProperties":
			if _, ok := value.(bool); !object || !ok {
				return nil, errUTCP
			}
			out[key] = value
		case "enum", "const":
			if object || kind != "string" && kind != "boolean" {
				return nil, errUTCP
			}
			values := []any{value}
			if key == "enum" {
				var ok bool
				values, ok = value.([]any)
				if !ok || len(values) == 0 {
					return nil, errUTCP
				}
			}
			for _, v := range values {
				switch kind {
				case "string":
					if _, ok := v.(string); !ok {
						return nil, errUTCP
					}
				case "boolean":
					if _, ok := v.(bool); !ok {
						return nil, errUTCP
					}
				}
			}
			out[key] = value
		case "minLength", "maxLength":
			n, ok := value.(json.Number)
			if kind != "string" || !ok {
				return nil, errUTCP
			}
			size, err := strconv.ParseInt(n.String(), 10, 64)
			if err != nil || size < 0 || size > maxRegistryBytes {
				return nil, errUTCP
			}
			out[key] = value
		case "pattern":
			if _, ok := value.(string); kind != "string" || !ok {
				return nil, errUTCP
			}
			out[key] = value
		default:
			return nil, errUTCP
		}
	}
	return out, nil
}

func utcpTemplate(source Source, template map[string]any) (string, error) {
	for key, value := range template {
		switch key {
		case "name":
			name, ok := value.(string)
			if !ok || !identifierPattern.MatchString(name) || !safeText(name, 128) {
				return "", errUTCP
			}
		case "call_template_type", "url", "http_method":
		case "content_type":
			if value != "application/json" {
				return "", errUTCP
			}
		case "body_field":
			if value != nil && value != "body" {
				return "", errUTCP
			}
		case "headers":
			if value != nil {
				v, ok := value.(map[string]any)
				if !ok || len(v) != 0 {
					return "", errUTCP
				}
			}
		case "header_fields":
			if value != nil {
				v, ok := value.([]any)
				if !ok || len(v) != 0 {
					return "", errUTCP
				}
			}
		case "auth":
			if value == nil {
				continue
			}
			auth, ok := value.(map[string]any)
			if !ok || len(auth) != 4 || source.CredentialEnv == "" || auth["auth_type"] != "api_key" || auth["api_key"] != "Bearer ${"+source.CredentialEnv+"}" || auth["var_name"] != "Authorization" || auth["location"] != "header" {
				return "", errUTCP
			}
		default:
			return "", errUTCP
		}
	}
	endpoint, ok := template["url"].(string)
	if !ok || template["call_template_type"] != "http" || template["http_method"] != "GET" || !validEndpoint(endpoint) || !utcpSafePath(endpoint) {
		return "", errUTCP
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return "", errUTCP
	}
	manual, err := url.Parse(source.Endpoint)
	if err != nil {
		return "", errUTCP
	}
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	if target.Scheme != manual.Scheme || !strings.EqualFold(target.Hostname(), manual.Hostname()) || port(target) != port(manual) || strings.ContainsAny(target.Host, "{}$") {
		return "", errUTCP
	}
	cleaned := utcpPlaceholder.ReplaceAllString(target.Path, "")
	if strings.ContainsAny(cleaned, "{}$") {
		return "", errUTCP
	}
	for _, match := range utcpPlaceholder.FindAllStringSubmatch(target.Path, -1) {
		if match[1] == "body" {
			return "", errUTCP
		}
	}
	return endpoint, nil
}

func utcpSafePath(value string) bool {
	for i := 0; i < 8; i++ {
		if !safeText(value, maxRegistryBytes) || strings.Contains(value, "\\") {
			return false
		}
		for _, part := range strings.Split(value, "/") {
			if part == "." || part == ".." {
				return false
			}
		}
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return false
		}
		if decoded == value {
			return true
		}
		value = decoded
	}
	return false
}

func (tool utcpTool) requestURL(ctx context.Context, raw []byte) (string, error) {
	var args map[string]any
	if safety.DecodeJSON(ctx, string(raw), &args) != nil {
		return "", errUTCP
	}
	scalars := map[string]string{}
	validation := map[string]any{}
	for key, value := range args {
		if key == "body" || !identifierPattern.MatchString(key) || !safeText(key, 128) {
			return "", errUTCP
		}
		switch v := value.(type) {
		case string:
			validation[key] = v
			scalars[key] = v
		case bool:
			validation[key] = v
			if v {
				scalars[key] = "true"
			} else {
				scalars[key] = "false"
			}
		case json.Number:
			number, err := utcpValidationNumber(v)
			if err != nil {
				return "", errUTCP
			}
			validation[key] = number
			scalars[key] = v.String()
		default:
			return "", errUTCP
		}
	}
	if tool.schema.Validate(validation) != nil {
		return "", errUTCP
	}
	target, err := url.Parse(strings.NewReplacer("{", "%7B", "}", "%7D").Replace(tool.endpoint))
	if err != nil {
		return "", errUTCP
	}
	path := utcpEscapedPlaceholder.ReplaceAllString(target.EscapedPath(), "{$1}")
	var escaped strings.Builder
	end := 0
	for _, match := range utcpPlaceholder.FindAllStringSubmatchIndex(path, -1) {
		escaped.WriteString(path[end:match[0]])
		key := path[match[2]:match[3]]
		value, ok := scalars[key]
		if !ok || !utcpSafePath(value) {
			return "", errUTCP
		}
		escaped.WriteString(url.PathEscape(value))
		end = match[1]
	}
	escaped.WriteString(path[end:])
	for _, match := range utcpPlaceholder.FindAllStringSubmatch(path, -1) {
		delete(scalars, match[1])
	}
	target.RawPath = escaped.String()
	if !utcpSafePath(target.RawPath) {
		return "", errUTCP
	}
	target.Path, err = url.PathUnescape(target.RawPath)
	if err != nil {
		return "", errUTCP
	}
	query := url.Values{}
	for key, value := range scalars {
		query.Set(key, value)
	}
	target.RawQuery = query.Encode()
	endpoint := target.String()
	if len(endpoint) > maxRegistryBytes {
		return "", errUTCP
	}
	return endpoint, nil
}

func (s Service) callUTCP(ctx context.Context, source Source, selected Tool, args []byte, result CallResult) (CallResult, error) {
	fail := func(reason string, cause ...error) (CallResult, error) {
		result.Gaps = append(result.Gaps, Gap{SourceID: source.ID, Reason: reason, Impact: "Source evidence is unavailable; the investigation remains incomplete."})
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if len(cause) > 0 && (errors.Is(cause[0], context.DeadlineExceeded) || errors.Is(cause[0], context.Canceled)) {
			return result, errors.Join(errors.New(reason), cause[0])
		}
		return result, errors.New(reason)
	}
	credential, err := s.utcpCredential(source)
	if err != nil {
		return fail("source connection or tool inventory unavailable", err)
	}
	tools, _, err := s.utcpManual(ctx, source, credential)
	if err != nil {
		return fail("source connection or tool inventory unavailable", err)
	}
	var selectedTool *utcpTool
	for _, tool := range tools {
		if tool.descriptor == selected {
			selectedTool = &tool
			break
		}
	}
	if selectedTool == nil {
		return fail("approved tool is missing or changed")
	}
	endpoint, err := selectedTool.requestURL(ctx, args)
	if err != nil {
		return fail("source arguments are invalid or unsupported")
	}
	raw, contentType, err := s.utcpGet(ctx, endpoint, credential)
	if err != nil {
		return fail("source collection failed", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fail("source collection returned no evidence")
	}
	media, _, _ := mime.ParseMediaType(contentType)
	if media == "application/json" || strings.HasSuffix(media, "+json") {
		var value any
		if utcpJSON(ctx, raw, &value) != nil || value == nil {
			return fail("source collection response invalid or too large")
		}
	} else {
		raw, err = json.Marshal(string(raw))
		if err != nil {
			return fail("source collection response invalid or too large")
		}
	}
	// Wrap scalars so the redactor preserves JSON quoting as it does for objects.
	wrapped, err := safety.NewRedactor(credential).RedactContext(ctx, "["+string(raw)+"]")
	var values []json.RawMessage
	if err != nil || json.Unmarshal([]byte(wrapped), &values) != nil || len(values) != 1 {
		return fail("source collection response invalid or too large")
	}
	redacted := string(values[0])
	if err != nil || len(redacted) > maxRegistryBytes || !json.Valid([]byte(redacted)) {
		return fail("source collection response invalid or too large")
	}
	if ctx.Err() != nil {
		return fail("source collection canceled")
	}
	result.Data = json.RawMessage(redacted)
	return result, nil
}

// jsonschema-go v0.4.3 classifies json.Number as a string for type checks.
// Convert only validation values; URL serialization retains the original lexeme.
func utcpValidationNumber(value json.Number) (any, error) {
	if len(value) > 128 {
		return nil, errUTCP
	}
	if i := strings.IndexAny(value.String(), "eE"); i >= 0 {
		exponent, err := strconv.Atoi(value.String()[i+1:])
		if err != nil || exponent < -324 || exponent > 308 {
			return nil, errUTCP
		}
	}
	if n, err := strconv.ParseInt(value.String(), 10, 64); err == nil {
		return n, nil
	}
	if n, err := strconv.ParseUint(value.String(), 10, 64); err == nil {
		return n, nil
	}
	n, err := strconv.ParseFloat(value.String(), 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
		return nil, errUTCP
	}
	exact, ok := new(big.Rat).SetString(value.String())
	if !ok {
		return nil, errUTCP
	}
	if exact.IsInt() {
		if exact.Num().IsInt64() {
			return exact.Num().Int64(), nil
		}
		if exact.Num().IsUint64() {
			return exact.Num().Uint64(), nil
		}
		return nil, errUTCP
	}
	if n == math.Trunc(n) {
		return nil, errUTCP
	}
	return n, nil
}

func utcpTransportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errUTCP
}
