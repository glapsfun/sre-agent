package safety

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MaxResponseBytes bounds individual synchronous JSON allocations. Oversized
// evidence is rejected explicitly, never parsed as a truncated document.
const MaxResponseBytes = 8 << 20

var ErrResponseTooLarge = errors.New("external response exceeds 8 MiB limit")

// Bound conversion work as well as bytes: small JSON objects allocate far more
// than a similarly sized string. This admits 16,000 small objects and the
// discovery 5,000-node fixture without an uninterruptible large conversion.
const MaxJSONStructure = 65536

var ErrResponseComplexity = errors.New("external JSON response exceeds 65536 structural-token limit")

func checkJSONWork(ctx context.Context, text string) error {
	quoted, escaped, count := false, false, 0
	for i := 0; i < len(text); i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if escaped {
			escaped = false
			continue
		}
		if quoted {
			switch text[i] {
			case '\\':
				escaped = true
			case '"':
				quoted = false
			}
			continue
		}
		switch text[i] {
		case '"':
			quoted = true
		case '{', '}', '[', ']', ',', ':':
			count++
			if count > MaxJSONStructure {
				return ErrResponseComplexity
			}
		}
	}
	return ctx.Err()
}

type contextReader struct {
	ctx    context.Context
	reader *strings.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > 4096 {
		p = p[:4096]
	}
	return r.reader.Read(p)
}
func (r *contextReader) ReadRune() (rune, int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, 0, err
	}
	return r.reader.ReadRune()
}

// DecodeJSON checks cancellation while scanning and rejects trailing values.
// Byte and structural-work limits bound the final synchronous conversion step.
func DecodeJSON(ctx context.Context, text string, dest any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(text) > MaxResponseBytes {
		return ErrResponseTooLarge
	}
	if err := checkJSONWork(ctx, text); err != nil {
		return err
	}
	decoder := json.NewDecoder(&contextReader{ctx: ctx, reader: strings.NewReader(text)})
	decoder.UseNumber()
	err := decoder.Decode(dest)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	var trailing any
	err = decoder.Decode(&trailing)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// RedactContext performs the foundation's redaction synchronously, checking the
// deadline inside regex scans and between JSON values. It starts no workers.
func (r Redactor) RedactContext(ctx context.Context, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(text) > MaxResponseBytes {
		return "", ErrResponseTooLarge
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var value any
		err := DecodeJSON(ctx, text, &value)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, ErrResponseComplexity) {
			return "", err
		}
		if err == nil {
			value, err = r.redactValueContext(ctx, value)
			if err != nil {
				return "", err
			}
			var out strings.Builder
			if err = writeJSONContext(ctx, &out, value); err != nil {
				return "", err
			}
			return out.String(), nil
		}
	}
	return r.redactTextContext(ctx, text)
}
func (r Redactor) redactValueContext(ctx context.Context, value any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		used := make(map[string]bool, len(v))
		names := make(map[string]string, len(v))
		for key := range v {
			name, err := r.redactTextContext(ctx, key)
			if err != nil {
				return nil, err
			}
			keys = append(keys, key)
			names[key] = name
			if name == key {
				used[key] = true
			}
		}
		sort.Strings(keys)
		redacted := make(map[string]any, len(v))
		for _, key := range keys {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name := names[key]
			if name != key {
				base := name
				for suffix := 2; used[name]; suffix++ {
					name = base + "#" + strconv.Itoa(suffix)
				}
				used[name] = true
			}
			sensitive := sensitiveKey.FindReaderIndex(&contextReader{ctx: ctx, reader: strings.NewReader(key)}) != nil
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if sensitive {
				redacted[name] = replacement
			} else {
				item, err := r.redactValueContext(ctx, v[key])
				if err != nil {
					return nil, err
				}
				redacted[name] = item
			}
		}
		return redacted, nil
	case []any:
		for i, item := range v {
			next, err := r.redactValueContext(ctx, item)
			if err != nil {
				return nil, err
			}
			v[i] = next
		}
	case string:
		return r.redactTextContext(ctx, v)
	case json.Number, bool, nil:
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		redacted, err := r.redactTextContext(ctx, string(encoded))
		if err != nil {
			return nil, err
		}
		if redacted != string(encoded) {
			return replacement, nil
		}
	}
	return value, nil
}
func (r Redactor) redactTextContext(ctx context.Context, text string) (string, error) {
	rules := []struct {
		pattern     *regexp.Regexp
		replacement string
	}{
		{privateKey, replacement}, {authorization, "${1}" + replacement}, {credentials, "${1}" + replacement + "@"},
	}
	for _, rule := range rules {
		var err error
		text, err = replaceContext(ctx, text, rule.pattern, func(input string, indices []int) string {
			return string(rule.pattern.ExpandString(nil, rule.replacement, input, indices))
		})
		if err != nil {
			return "", err
		}
	}
	var err error
	text, err = replaceContext(ctx, text, assignment, func(input string, indices []int) string {
		prefix := input[indices[2]:indices[3]]
		value := input[indices[4]:indices[5]]
		if strings.HasPrefix(value, "\"") {
			return prefix + "\"" + replacement + "\""
		}
		if strings.HasPrefix(value, "'") {
			return prefix + "'" + replacement + "'"
		}
		return prefix + replacement
	})
	if err != nil {
		return "", err
	}
	for _, secret := range r.secrets {
		var out strings.Builder
		for len(text) > 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			index := strings.Index(text, secret)
			if index < 0 {
				out.WriteString(text)
				break
			}
			out.WriteString(text[:index])
			out.WriteString(replacement)
			text = text[index+len(secret):]
		}
		text = out.String()
	}
	return text, ctx.Err()
}
func replaceContext(ctx context.Context, text string, pattern *regexp.Regexp, replace func(string, []int) string) (string, error) {
	var out strings.Builder
	for len(text) > 0 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		match := pattern.FindReaderSubmatchIndex(&contextReader{ctx: ctx, reader: strings.NewReader(text)})
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if match == nil {
			out.WriteString(text)
			break
		}
		out.WriteString(text[:match[0]])
		out.WriteString(replace(text, match))
		text = text[match[1]:]
	}
	return out.String(), ctx.Err()
}

// Encode each string in bounded pieces so a single long external value cannot
// monopolize the deadline after redaction. JSON structure is retained intact.
func writeJSONContext(ctx context.Context, out *strings.Builder, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeJSONContext(ctx, out, key); err != nil {
				return err
			}
			out.WriteByte(':')
			if err := writeJSONContext(ctx, out, v[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeJSONContext(ctx, out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case string:
		out.WriteByte('"')
		for len(v) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := min(4096, len(v))
			for end < len(v) && v[end]&0xc0 == 0x80 {
				end--
			}
			// Invalid UTF-8 is replaced by encoding/json, as in the ordinary redactor.
			if end == 0 {
				end = min(4096, len(v))
			}
			encoded, err := json.Marshal(v[:end])
			if err != nil {
				return err
			}
			out.Write(encoded[1 : len(encoded)-1])
			v = v[end:]
		}
		out.WriteByte('"')
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out.Write(encoded)
	}
	return ctx.Err()
}
