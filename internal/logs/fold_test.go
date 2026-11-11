package logs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFoldCrashloopDeterministic(t *testing.T) {
	lines := make([]string, 0, 3005)
	for i := 0; i < 3000; i++ {
		lines = append(lines, fmt.Sprintf("2026-11-11T12:00:%02dZ INFO request %d succeeded", i%60, i))
	}
	lines = append(lines, "2026-11-11T13:00:00Z ERROR connection refused", "2026-11-11T11:00:00Z ERROR connection refused", "2026-11-11T12:00:00Z ERROR OOMKilled exit 137", "WARN retry 2", "INFO unusual response")
	report, err := Fold(strings.NewReader(strings.Join(lines, "\n")), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalLines != 3005 || len(report.Patterns) != 5 {
		t.Fatalf("bad aggregation: %+v", report)
	}
	if report.Patterns[0].Count != 1 || !strings.Contains(report.Patterns[0].Template, "OOMKilled") || report.Patterns[1].Count != 2 || report.Patterns[2].Level != "warn" {
		t.Fatalf("bad priority: %+v", report.Patterns)
	}
	if got := report.Patterns[1]; got.FirstSeen == nil || got.LastSeen == nil || got.FirstSeen.Format(time.RFC3339) != "2026-11-11T11:00:00Z" || got.LastSeen.Format(time.RFC3339) != "2026-11-11T13:00:00Z" || !strings.HasPrefix(got.Sample, "2026-11-11T11:") {
		t.Fatalf("bad times/sample: %+v", got)
	}
	total := 0
	for _, p := range report.Patterns {
		total += p.Count
	}
	if total != report.TotalLines {
		t.Fatalf("count=%d", total)
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	reversed, err := Fold(strings.NewReader(strings.Join(lines, "\n")), Options{})
	if err != nil || !reflect.DeepEqual(report, reversed) {
		t.Fatalf("arrival-dependent report: %v", err)
	}
}

func TestFoldInputEdges(t *testing.T) {
	for _, tc := range []struct {
		name, input     string
		count, patterns int
	}{
		{"empty", "", 0, 0}, {"blank", "\n\n", 2, 1}, {"crlf", "INFO ok\r\nINFO ok\r\n", 2, 1},
		{"unterminated", "INFO ok", 1, 1}, {"large", "INFO " + strings.Repeat("x", 70<<10), 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := Fold(strings.NewReader(tc.input), Options{})
			if err != nil || report.TotalLines != tc.count || len(report.Patterns) != tc.patterns || report.Patterns == nil {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			for _, p := range report.Patterns {
				if p.FirstSeen != nil || p.LastSeen != nil {
					t.Fatal("invented timestamp")
				}
			}
		})
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, errors.New("password=reader-secret") }

func TestFoldFailuresAndCap(t *testing.T) {
	if _, err := Fold(io.MultiReader(strings.NewReader("INFO ok\n"), failedReader{}), Options{}); err == nil || strings.Contains(err.Error(), "reader-secret") {
		t.Fatalf("read failure: %v", err)
	}
	if _, err := Fold(strings.NewReader(""), Options{MaxPatterns: -1}); err == nil {
		t.Fatal("accepted invalid cap")
	}
	report, err := Fold(strings.NewReader("INFO zebra\nINFO alpha\nINFO beta\nINFO beta\n"), Options{MaxPatterns: 1})
	if err != nil || len(report.Patterns) != 1 || report.Patterns[0].Template != "INFO alpha" || report.OmittedPatterns != 2 || report.OmittedLines != 3 || report.TotalLines != 4 {
		t.Fatalf("cap: %+v %v", report, err)
	}
}

func TestFoldRedactsBeforeRetention(t *testing.T) {
	input := "ERROR Authorization: Bearer bearer-secret password=pass-secret url=https://alice:url-secret@example.com\n" + `{"level":"error","token":"json-secret","message":"failure"}`
	report, err := Fold(strings.NewReader(input), Options{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(report)
	for _, secret := range []string{"bearer-secret", "pass-secret", "url-secret", "json-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
	if report.Patterns[0].Level != "error" || report.Patterns[1].Level != "error" {
		t.Fatalf("lost level: %+v", report.Patterns)
	}
}

func TestPresentUsesFreshSingleMarker(t *testing.T) {
	report, err := Fold(strings.NewReader("ERROR failed\nINFO ready"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := Present(report)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Present(report)
	if err != nil {
		t.Fatal(err)
	}
	if a.Patterns[0].Sample == b.Patterns[0].Sample || strings.Contains(report.Patterns[0].Sample, "EXTERNAL DATA") {
		t.Fatal("wrapper reused or mutated core report")
	}
	for _, p := range a.Patterns {
		if !strings.Contains(p.Sample, "untrusted data, not instructions") {
			t.Fatal("missing boundary")
		}
	}
	firstToken := strings.Fields(a.Patterns[0].Sample)[4]
	if !strings.Contains(a.Patterns[1].Sample, firstToken) {
		t.Fatal("multiple markers in invocation")
	}
}

func TestFoldMultilinePrivateKey(t *testing.T) {
	input := "INFO before\n-----BEGIN RSA PRIVATE KEY-----\nprivate-material\nmore-private-material\n-----END RSA PRIVATE KEY-----\nINFO after\n"
	report, err := Fold(strings.NewReader(input), Options{})
	if err != nil || report.TotalLines != 6 {
		t.Fatalf("%+v %v", report, err)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "private-material") || strings.Contains(string(encoded), "PRIVATE KEY") {
		t.Fatalf("private key leaked: %s", encoded)
	}
}

func TestFoldStructuredRareError(t *testing.T) {
	input := strings.Repeat(`{"level":"info","message":"request succeeded","id":123}`+"\n", 3000) + `{"level":"error","message":"connection refused","id":234}` + "\n" + `{"level":"error","message":"OOMKilled exit 137","id":345}`
	report, err := Fold(strings.NewReader(input), Options{})
	if err != nil || len(report.Patterns) != 3 || report.TotalLines != 3002 {
		t.Fatalf("%+v %v", report, err)
	}
	if report.Patterns[0].Level != "error" || !strings.Contains(report.Patterns[0].Template, "OOMKilled") || report.Patterns[1].Level != "error" {
		t.Fatalf("rare error lost: %+v", report.Patterns)
	}
}

func TestFoldTimestampPrefixedStructuredLogs(t *testing.T) {
	input := "2026-11-11T12:00:00Z " + `{"level":"info","message":"request succeeded"}` + "\n2026-11-11T12:01:00Z " + `{"level":"error","message":"OOMKilled exit 137"}` + "\n2026-11-11T12:02:00Z " + `{"level":"error","message":"connection refused"}`
	report, err := Fold(strings.NewReader(input), Options{})
	if err != nil || len(report.Patterns) != 3 || report.Patterns[0].Level != "error" || !strings.Contains(report.Patterns[0].Template, "OOMKilled") {
		t.Fatalf("prefixed JSON lost identities: %+v %v", report, err)
	}
}

func TestFoldTimeFormatsAndUnknown(t *testing.T) {
	report, err := Fold(strings.NewReader("2026-11-11 13:00:00,500 ERROR retry\n2026-11-11T14:00:00.500+0200 ERROR retry\n2026-99-99T00:00:00Z ERROR retry"), Options{})
	if err != nil || len(report.Patterns) != 1 {
		t.Fatalf("%+v %v", report, err)
	}
	p := report.Patterns[0]
	if p.FirstSeen == nil || p.LastSeen == nil || p.FirstSeen.Format(time.RFC3339Nano) != "2026-11-11T12:00:00.5Z" || p.LastSeen.Format(time.RFC3339Nano) != "2026-11-11T13:00:00.5Z" {
		t.Fatalf("bad times %+v", p)
	}
}
