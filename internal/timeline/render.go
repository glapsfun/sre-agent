package timeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/safety"
)

// RedactReport handles projected fields individually so reference schema paths
// such as secretKeyRef are not mistaken for secret-bearing JSON keys.
func RedactReport(ctx context.Context, report Report) (Report, error) {
	result, err := cleanSource(ctx, SourceResult{Entries: report.Entries, Gaps: report.Gaps, Errors: report.Errors})
	if err != nil {
		return Report{}, err
	}
	report.Entries, report.Gaps, report.Errors = result.Entries, result.Gaps, result.Errors
	report.Sources = append([]SourceSummary(nil), report.Sources...)
	for i, source := range report.Sources {
		clean, err := cleanSource(ctx, SourceResult{Summary: source})
		if err != nil {
			return Report{}, err
		}
		report.Sources[i] = clean.Summary
	}
	var failure error
	for _, value := range []*string{&report.Context, &report.Namespace, &report.Workload} {
		if failure == nil {
			*value, failure = safety.NewRedactor().RedactContext(ctx, *value)
		}
	}
	return report, failure
}

func cleanSource(ctx context.Context, result SourceResult) (SourceResult, error) {
	redactor := safety.NewRedactor()
	var failure error
	text := func(value string) string {
		if failure != nil {
			return ""
		}
		var clean string
		clean, failure = redactor.RedactContext(ctx, value)
		return clean
	}
	object := func(obj Object) Object {
		return Object{APIVersion: text(obj.APIVersion), Kind: text(obj.Kind), Namespace: text(obj.Namespace), Name: text(obj.Name), UID: text(obj.UID)}
	}
	var value func(any) any
	value = func(input any) any {
		if failure != nil {
			return nil
		}
		if failure = collectionError(ctx); failure != nil {
			return nil
		}
		switch v := input.(type) {
		case string:
			return text(v)
		case Object:
			return object(v)
		case map[string]any:
			out := make(map[string]any, len(v))
			for key, item := range v {
				out[key] = value(item)
			}
			return out
		case []any:
			out := make([]any, len(v))
			for i, item := range v {
				out[i] = value(item)
			}
			return out
		case []string:
			out := make([]string, len(v))
			for i, item := range v {
				out[i] = text(item)
			}
			return out
		case []FieldChange:
			out := make([]FieldChange, len(v))
			for i, item := range v {
				out[i] = FieldChange{Path: item.Path, Before: value(item.Before), After: value(item.After)}
			}
			return out
		default:
			return input
		}
	}
	result.Entries = append([]Entry{}, result.Entries...)
	for i, entry := range result.Entries {
		entry.ID, entry.TimeBasis, entry.Change, entry.Command = text(entry.ID), text(entry.TimeBasis), text(entry.Change), text(entry.Command)
		entry.Object = object(entry.Object)
		if entry.Details != nil {
			clean := value(entry.Details)
			if failure != nil {
				return SourceResult{}, failure
			}
			entry.Details = clean.(map[string]any)
			if entry.Source == "replicasets" {
				if template, ok := entry.Details["template"].(map[string]any); ok {
					for path, item := range template {
						template[path] = cleanTemplateValue(ctx, path, item, &failure)
					}
				}
				if changes, ok := entry.Details["changes"].([]FieldChange); ok {
					for i, change := range changes {
						changes[i].Before = cleanTemplateValue(ctx, change.Path, change.Before, &failure)
						changes[i].After = cleanTemplateValue(ctx, change.Path, change.After, &failure)
					}
				}
			}
		}
		result.Entries[i] = entry
		if failure != nil {
			return SourceResult{}, failure
		}
	}
	if result.Summary.Context != nil {
		clean := value(result.Summary.Context)
		if failure != nil {
			return SourceResult{}, failure
		}
		result.Summary.Context = clean.(map[string]any)
	}
	result.Gaps = append([]Gap{}, result.Gaps...)
	for i, gap := range result.Gaps {
		gap.Message = text(gap.Message)
		result.Gaps[i] = gap
	}
	result.Errors = append([]string{}, result.Errors...)
	for i, message := range result.Errors {
		result.Errors[i] = text(message)
	}
	return result, failure
}

func cleanTemplateValue(ctx context.Context, path string, item any, failure *error) any {
	if *failure != nil {
		return nil
	}
	// These schema locations contain external map keys. Other segments are fixed
	// schema or validated reference identity and must remain readable.
	key, found, err := templateDynamicKey(ctx, path)
	if err != nil {
		*failure = err
		return nil
	}
	if found {
		raw, err := json.Marshal(map[string]any{key: item})
		if err != nil {
			*failure = err
			return nil
		}
		clean, err := safety.NewRedactor().RedactContext(ctx, string(raw))
		if err != nil {
			*failure = err
			return nil
		}
		var decoded map[string]any
		if err = json.Unmarshal([]byte(clean), &decoded); err != nil {
			*failure = err
			return nil
		}
		return decoded[key]
	}
	return item
}

func templateDynamicKey(ctx context.Context, path string) (string, bool, error) {
	depth := 0
	quoted, escaped := false, false
	for i := 0; i < len(path); i++ {
		if i%4096 == 0 {
			if err := collectionError(ctx); err != nil {
				return "", false, err
			}
		}
		char := path[i]
		if quoted {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				quoted = false
			}
			continue
		}
		if depth > 0 {
			switch char {
			case '"':
				quoted = true
			case '[':
				depth++
			case ']':
				depth--
			}
			continue
		}
		if char == '[' {
			depth++
			continue
		}
		if char != '.' {
			continue
		}
		for _, segment := range []string{".nodeSelector.", ".matchLabels.", ".limits.", ".requests."} {
			if strings.HasPrefix(path[i:], segment) {
				return path[i+len(segment):], true, nil
			}
		}
	}
	return "", false, nil
}

func RenderText(report Report, wrapper *safety.Wrapper) (string, error) {
	if wrapper == nil {
		return "", errors.New("timeline output wrapper is required")
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Range: %s to %s\nContext: %s, namespace: %s, workload: %s\n", report.Start.Format(time.RFC3339Nano), report.End.Format(time.RFC3339Nano), report.Context, report.Namespace, report.Workload)
	for _, entry := range report.Entries {
		name := entry.Object.Kind + "/" + entry.Object.Name
		if entry.Object.Namespace != "" {
			name = entry.Object.Namespace + "/" + name
		}
		fmt.Fprintf(&out, "%s\t%s\t%s\t%s\n", entry.Time.Format(time.RFC3339Nano), entry.Source, name, entry.Change)
		if len(entry.Details) > 0 {
			details, err := json.Marshal(entry.Details)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&out, "  details: %s\n", details)
		}
	}
	for _, source := range report.Sources {
		fmt.Fprintf(&out, "Source %s: %s, observed %d, matched %d\n", source.Source, source.Status, source.Observed, source.Matched)
		if len(source.Context) > 0 {
			data, err := json.Marshal(source.Context)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&out, "  undated context: %s\n", data)
		}
	}
	for _, gap := range report.Gaps {
		fmt.Fprintf(&out, "Gap %s (%s, count %d): %s\n", gap.Source, gap.Code, gap.Count, gap.Message)
	}
	for _, message := range report.Errors {
		fmt.Fprintf(&out, "Error: %s\n", message)
	}
	return wrapper.Wrap("timeline", out.String()), nil
}
