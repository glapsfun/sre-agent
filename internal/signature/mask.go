// Package signature provides the shared log and incident fingerprint masks.
package signature

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"regexp"
	"strings"
)

var (
	quoted      = regexp.MustCompile(`"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'`)
	timestamp   = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	uuid        = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	podSuffix   = regexp.MustCompile(`\b([a-z0-9][a-z0-9-]*)-[a-z0-9]{8,10}-[a-z0-9]{5}\b`)
	ipCandidate = regexp.MustCompile(`[0-9a-fA-F:.]+(?:%[a-zA-Z0-9_-]+)?`)
	hex         = regexp.MustCompile(`(?i)\b(?:0x[0-9a-f]+|[0-9a-f]{8,})\b`)
	number      = regexp.MustCompile(`[+-]?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`)
)

// Mask retains static words while replacing common variable fields. Structured
// logs keep their keys, level, and masked message so distinct failures survive.
func Mask(text string) string {
	if match := timestamp.FindStringIndex(text); match != nil && match[0] == 0 {
		rest := strings.TrimSpace(text[match[1]:])
		if strings.HasPrefix(rest, "{") && json.Valid([]byte(rest)) {
			return "<*> " + Mask(rest)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(text), &fields) == nil && fields != nil {
			masked := make(map[string]string, len(fields))
			for key, raw := range fields {
				var value string
				if json.Unmarshal(raw, &value) == nil {
					switch strings.ToLower(key) {
					case "message", "msg":
						value = maskText(value)
					case "level", "severity":
					default:
						value = "<*>"
					}
					masked[key] = value
				} else {
					masked[key] = "<*>"
				}
			}
			var encoded bytes.Buffer
			encoder := json.NewEncoder(&encoded)
			encoder.SetEscapeHTML(false)
			_ = encoder.Encode(masked)
			return strings.TrimSuffix(encoded.String(), "\n")
		}
	}
	return maskText(text)
}

func maskText(text string) string {
	text = quoted.ReplaceAllString(text, "<*>")
	text = timestamp.ReplaceAllString(text, "<*>")
	text = uuid.ReplaceAllString(text, "<*>")
	text = podSuffix.ReplaceAllString(text, "${1}-<*>")
	text = ipCandidate.ReplaceAllStringFunc(text, func(candidate string) string {
		if _, err := netip.ParseAddr(candidate); err == nil {
			return "<*>"
		}
		// A dotted IPv4 address may carry a port in the same token.
		if addr, err := netip.ParseAddrPort(candidate); err == nil && addr.Addr().Is4() {
			return "<*>:<*>"
		}
		return candidate
	})
	text = hex.ReplaceAllString(text, "<*>")
	return number.ReplaceAllString(text, "<*>")
}
