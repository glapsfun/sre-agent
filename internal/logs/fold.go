// Package logs folds redacted log lines into deterministic patterns.
package logs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/signature"
)

const DefaultMaxPatterns = 20

type Options struct{ MaxPatterns int }

type Pattern struct {
	Template  string     `json:"template"`
	Count     int        `json:"count"`
	FirstSeen *time.Time `json:"first_seen"`
	LastSeen  *time.Time `json:"last_seen"`
	Sample    string     `json:"sample"`
	Level     string     `json:"level"`
}

type Report struct {
	Patterns        []Pattern `json:"patterns"`
	TotalLines      int       `json:"total_lines"`
	OmittedPatterns int       `json:"omitted_patterns"`
	OmittedLines    int       `json:"omitted_lines"`
}

var timeField = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
var levelField = regexp.MustCompile(`(?i)\b(FATAL|PANIC|ERROR|ERR|WARNING|WARN|INFO|DEBUG|TRACE)\b`)
var keyBegin = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
var keyEnd = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)

// Fold streams input and retains one redacted sample per distinct signature.
// Zero MaxPatterns uses the default; negative limits are rejected.
func Fold(input io.Reader, opts Options) (Report, error) {
	report := Report{Patterns: []Pattern{}}
	if opts.MaxPatterns < 0 {
		return report, errors.New("max-patterns must be positive")
	}
	if opts.MaxPatterns == 0 {
		opts.MaxPatterns = DefaultMaxPatterns
	}
	patterns := map[string]*Pattern{}
	redactor := safety.NewRedactor()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), safety.MaxResponseBytes)
	inPrivateKey := false
	for scanner.Scan() {
		line := scanner.Text()
		report.TotalLines++
		// PEM bodies cross line boundaries; suppress each line while preserving
		// the original count, including an unterminated key at end of input.
		if inPrivateKey || keyBegin.MatchString(line) {
			inPrivateKey = !keyEnd.MatchString(line)
			line = "[REDACTED]"
		}
		prefix, message := splitTimestamp(line)
		line = prefix + redactor.Redact(message)
		template, level := signature.Mask(line), parseLevel(line)
		key := template + "\x00" + level
		pattern := patterns[key]
		if pattern == nil {
			pattern = &Pattern{Template: template, Sample: line, Level: level}
			patterns[key] = pattern
		}
		pattern.Count++
		if line < pattern.Sample {
			pattern.Sample = line
		}
		if stamp := parseTime(line); stamp != nil {
			if pattern.FirstSeen == nil || stamp.Before(*pattern.FirstSeen) {
				pattern.FirstSeen = stamp
			}
			if pattern.LastSeen == nil || stamp.After(*pattern.LastSeen) {
				pattern.LastSeen = stamp
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Report{}, fmt.Errorf("read logs: %s", redactor.Redact(err.Error()))
	}
	for _, pattern := range patterns {
		report.Patterns = append(report.Patterns, *pattern)
	}
	sort.Slice(report.Patterns, func(i, j int) bool {
		a, b := report.Patterns[i], report.Patterns[j]
		if severity(a.Level) != severity(b.Level) {
			return severity(a.Level) < severity(b.Level)
		}
		if a.Count != b.Count {
			return a.Count < b.Count
		}
		if a.Template != b.Template {
			return a.Template < b.Template
		}
		return a.Level < b.Level
	})
	if len(report.Patterns) > opts.MaxPatterns {
		report.OmittedPatterns = len(report.Patterns) - opts.MaxPatterns
		for _, p := range report.Patterns[opts.MaxPatterns:] {
			report.OmittedLines += p.Count
		}
		report.Patterns = report.Patterns[:opts.MaxPatterns]
	}
	return report, nil
}

func parseTime(line string) *time.Time {
	value := strings.Replace(timeField.FindString(line), " ", "T", 1)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05"} {
		if stamp, err := time.Parse(layout, value); err == nil {
			stamp = stamp.UTC()
			return &stamp
		}
	}
	return nil
}

func parseLevel(line string) string {
	_, line = splitTimestamp(line)
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &fields) == nil {
		for _, key := range []string{"level", "severity"} {
			var level string
			if json.Unmarshal(fields[key], &level) == nil {
				return normalizeLevel(level)
			}
		}
	}
	return normalizeLevel(levelField.FindString(line))
}

func splitTimestamp(line string) (string, string) {
	if match := timeField.FindStringIndex(line); match != nil && match[0] == 0 {
		return line[:match[1]] + " ", strings.TrimSpace(line[match[1]:])
	}
	return "", line
}

func normalizeLevel(level string) string {
	switch strings.ToLower(level) {
	case "fatal", "panic":
		return "fatal"
	case "error", "err":
		return "error"
	case "warning", "warn":
		return "warn"
	case "info", "debug", "trace":
		return strings.ToLower(level)
	default:
		return ""
	}
}

func severity(level string) int {
	switch level {
	case "fatal", "error":
		return 0
	case "warn":
		return 1
	default:
		return 2
	}
}

// Present adds one fresh external-data boundary to all samples and templates
// without mutating the deterministic report consumed by other services.
func Present(report Report) (Report, error) {
	wrapper, err := safety.NewWrapper()
	if err != nil {
		return Report{}, err
	}
	report.Patterns = append([]Pattern{}, report.Patterns...)
	for i := range report.Patterns {
		report.Patterns[i].Sample = wrapper.Wrap("log sample", report.Patterns[i].Sample)
		report.Patterns[i].Template = wrapper.Wrap("log template", report.Patterns[i].Template)
	}
	return report, nil
}
