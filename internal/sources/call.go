package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type CallOptions struct {
	SourceID  string
	Tool      string
	Arguments json.RawMessage
	Timeout   time.Duration
}

type CallResult struct {
	SourceID  string          `json:"source_id"`
	Tool      string          `json:"tool"`
	Reference string          `json:"reference"`
	At        time.Time       `json:"at"`
	Data      json.RawMessage `json:"data,omitempty"`
	Gaps      []Gap           `json:"gaps"`
}

// ValidateCallOptions lets callers reject invalid input before persistence or contact.
func ValidateCallOptions(opts CallOptions) error {
	arguments := opts.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if !identifierPattern.MatchString(opts.SourceID) || !identifierPattern.MatchString(opts.Tool) || opts.Timeout < 0 || opts.Timeout > 60*time.Second || !validCallArguments(arguments) {
		return errors.New("invalid source call options")
	}
	return nil
}

func (s Service) Call(ctx context.Context, root string, opts CallOptions) (CallResult, error) {
	result := CallResult{Gaps: []Gap{}}
	if identifierPattern.MatchString(opts.SourceID) {
		result.SourceID = opts.SourceID
	}
	if identifierPattern.MatchString(opts.Tool) {
		result.Tool = opts.Tool
	}
	fail := func(reason, impact string) (CallResult, error) {
		result.Gaps = append(result.Gaps, Gap{SourceID: result.SourceID, Reason: reason, Impact: impact})
		if err := ctx.Err(); err != nil {
			return result, err
		}
		return result, errors.New(reason)
	}
	arguments := opts.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if ValidateCallOptions(opts) != nil {
		return fail("invalid source call options", "Source evidence was not collected because the call options are invalid.")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if ctx.Err() != nil {
		return fail("source collection canceled", "Source evidence was not collected.")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	at := now().UTC()
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return fail("invalid source collection time", "Source evidence cannot be attributed to a valid collection time.")
	}
	result.At = at
	registry, err := Load(ctx, root)
	if err != nil {
		return fail("approved source registry unavailable", "Source evidence was not collected because saved approval could not be verified.")
	}
	var source *Source
	for _, approved := range registry.Sources {
		if approved.ID == opts.SourceID {
			source = &approved.Source
			break
		}
	}
	if source == nil {
		return fail("source is not approved", "Source evidence was not collected; explicit source approval is required.")
	}
	result.Reference = source.Reference
	if result.Reference == "" {
		result.Reference = source.Endpoint
	}
	if source.Kind != "mcp-http" && source.Kind != "utcp-http" {
		return fail("source integration needed", "This reference-only source requires an integration before evidence can be collected.")
	}
	var selected *Tool
	for _, tool := range source.Tools {
		if tool.Name == opts.Tool && tool.ReadOnly {
			selected = &tool
			break
		}
	}
	if selected == nil {
		return fail("tool is not approved for read-only collection", "Source evidence was not collected; explicit read-only tool approval is required.")
	}
	if source.Kind == "utcp-http" {
		return s.callUTCP(ctx, *source, *selected, arguments, result)
	}
	lookup := s.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	credential := ""
	// Capture the session's lookup rather than reading a possibly rotated value again.
	s.LookupEnv = func(key string) (string, bool) {
		value, ok := lookup(key)
		if ok {
			credential = value
		}
		return value, ok
	}
	session, tools, cleanup, err := s.openSession(ctx, *source)
	if err != nil {
		return fail("source connection or tool inventory unavailable", "Source evidence was not collected because the approved source could not be verified.")
	}
	defer cleanup()
	matched := false
	for _, tool := range tools {
		if tool == *selected && tool.ReadOnly {
			matched = true
			break
		}
	}
	if !matched {
		return fail("approved tool is missing or changed", "Source evidence was not collected; review the current tool and renew approval.")
	}
	response, err := session.CallTool(ctx, &mcp.CallToolParams{Name: opts.Tool, Arguments: arguments})
	if err != nil || response == nil || response.NeedsInput() {
		return fail("source collection failed", "Source evidence is unavailable; the investigation remains incomplete.")
	}
	// The SDK normalizes null and missing results to empty content too.
	if len(response.Content) == 0 && response.StructuredContent == nil {
		if response.IsError {
			return fail("source collection failed", "Source evidence is unavailable; the tool reported a collection failure.")
		}
		return fail("source collection returned no evidence", "Source evidence is unavailable; an explicit structured result or content is required.")
	}
	raw, err := json.Marshal(response)
	if err != nil || len(raw) > maxRegistryBytes {
		return fail("source collection response invalid or too large", "Source evidence was discarded; the investigation remains incomplete.")
	}
	var envelope struct {
		ResultType string `json:"resultType"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.ResultType != "" && envelope.ResultType != "complete" {
		return fail("source collection response invalid or too large", "Source evidence was discarded; the investigation remains incomplete.")
	}
	redacted := safety.NewRedactor(credential).Redact(string(raw))
	if len(redacted) > maxRegistryBytes || !json.Valid([]byte(redacted)) {
		return fail("source collection response invalid or too large", "Source evidence was discarded; the investigation remains incomplete.")
	}
	result.Data = json.RawMessage(redacted)
	if response.IsError {
		return fail("source collection failed", "Source evidence is partial and may be incomplete; the tool reported a collection failure.")
	}
	if ctx.Err() != nil {
		return fail("source collection canceled", "Source evidence may be incomplete because collection was interrupted.")
	}
	return result, nil
}

func validCallArguments(raw []byte) bool {
	if len(raw) > 64<<10 || !utf8.Valid(raw) || !json.Valid(raw) || bytes.TrimSpace(raw)[0] != '{' {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if !uniqueCallFields(decoder) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func uniqueCallFields(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return true
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] {
				return false
			}
			seen[key] = true
		}
		if !uniqueCallFields(decoder) {
			return false
		}
	}
	_, err = decoder.Token()
	return err == nil
}
