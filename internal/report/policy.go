package report

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/glapsfun/sre-agent/internal/safety"
)

type Rule struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Action  string `json:"action"`
}

type compiledRule struct {
	Rule
	expression *regexp.Regexp
}
type policy struct {
	rules []compiledRule
	flags map[string]bool
}

var policyName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var fieldURL = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)

func newPolicy(rules []Rule) (*policy, error) {
	if len(rules) > 64 {
		return nil, errors.New("report policy exceeds 64 rules")
	}
	p := &policy{flags: map[string]bool{}}
	names := map[string]bool{}
	for _, rule := range rules {
		if !policyName.MatchString(rule.Name) || names[rule.Name] || len(rule.Pattern) == 0 || len(rule.Pattern) > 4096 || (rule.Action != "redact" && rule.Action != "flag") {
			return nil, errors.New("invalid report policy rule")
		}
		expression, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return nil, errors.New("invalid report policy regular expression")
		}
		names[rule.Name] = true
		p.rules = append(p.rules, compiledRule{rule, expression})
	}
	return p, nil
}

func ReadPolicy(input io.Reader) ([]Rule, error) {
	limited := &io.LimitedReader{R: input, N: (256 << 10) + 1}
	var document struct {
		Rules *[]Rule `json:"rules"`
	}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil || limited.N <= 0 || document.Rules == nil {
		return nil, errors.New("report policy must be a JSON rules object within 256 KiB")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || limited.N <= 0 {
		return nil, errors.New("report policy must contain one JSON rules object within 256 KiB")
	}
	if _, err := newPolicy(*document.Rules); err != nil {
		return nil, err
	}
	return *document.Rules, nil
}

func (p *policy) clean(value string) string {
	value = p.plain(value)
	value = fieldURL.ReplaceAllStringFunc(value, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[REDACTED URL]"
		}
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return "[REDACTED URL]"
		}
		filtered := url.Values{}
		for key, values := range query {
			for _, value := range values {
				// Query keys carry credential semantics that free-text redaction
				// cannot detect after percent-encoding (for example Authorization).
				encoded, _ := json.Marshal(map[string]string{key: value})
				var safe map[string]string
				if json.Unmarshal([]byte(safety.NewRedactor().Redact(string(encoded))), &safe) != nil {
					return "[REDACTED URL]"
				}
				for name, text := range safe {
					filtered.Add(p.plain(name), p.plain(text))
				}
			}
		}
		u.RawQuery = filtered.Encode()
		u.Host = p.plain(u.Host)
		u.Path = p.plain(u.Path)
		u.RawPath = ""
		u.Fragment = p.plain(u.Fragment)
		u.RawFragment = ""
		if u.User != nil {
			u.User = url.User("[REDACTED]")
		}
		return u.String()
	})
	return p.plain(value)
}

func (p *policy) plain(value string) string {
	value = safety.NewRedactor().Redact(value)
	// Flags inspect the same text regardless of redaction-rule ordering.
	for _, rule := range p.rules {
		if rule.Action == "flag" && rule.expression.MatchString(value) {
			p.flags[rule.Name] = true
		}
	}
	for _, rule := range p.rules {
		if rule.Action == "redact" {
			value = rule.expression.ReplaceAllString(value, "[REDACTED]")
		}
	}
	return safety.NewRedactor().Redact(value)
}

func (p *policy) flagged() []string {
	flags := make([]string, 0, len(p.flags))
	for name := range p.flags {
		flags = append(flags, name)
	}
	sort.Strings(flags)
	return flags
}

func (p *policy) review() string {
	if len(p.flags) == 0 {
		return ""
	}
	return "\nPrivacy review flags: " + strings.Join(p.flagged(), ", ") + ".\n"
}
