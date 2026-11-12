package report

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/glapsfun/sre-agent/internal/ledger"
)

func setReportPayloadField(t *testing.T, entry *ledger.Entry, field string, value any) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(field, "criteria.") {
		payload["criteria"].([]any)[0].(map[string]any)[strings.TrimPrefix(field, "criteria.")] = value
	} else {
		payload[field] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	entry.Payload = raw
}

func TestReportSecretsAbsent(t *testing.T) {
	seeds := []struct {
		name, text string
		values     []string
	}{
		{"bearer", "Authorization: Bearer plantedBearerValue", []string{"plantedBearerValue"}},
		{"password", "password=plantedPasswordValue", []string{"plantedPasswordValue"}},
		{"private key", "-----BEGIN PRIVATE KEY-----\nplantedPrivateKeyValue\n-----END PRIVATE KEY-----", []string{"plantedPrivateKeyValue"}},
		{"credential URL", "https://plantedUser:plantedURLPassword@example.test/incident", []string{"plantedUser", "plantedURLPassword"}},
	}
	fields := []struct {
		entry int
		field string
	}{
		{0, "problem"}, {0, "impact"}, {0, "detection"}, {0, "links"},
		{1, "fact"}, {1, "source"}, {1, "links"},
		{2, "statement"}, {2, "mechanism"},
		{3, "statement"}, {3, "mechanism"}, {3, "discriminating_test"},
		{4, "criteria.description"}, {4, "criteria.observation"}, {4, "criteria.threshold"}, {4, "criteria.source"},
		{5, "summary"}, {5, "rollback"},
		{7, "command"}, {7, "result"}, {7, "rollback"},
		{8, "fact"}, {8, "source"},
		{9, "observed"}, {9, "source"}, {9, "reason"},
	}
	for _, seed := range seeds {
		t.Run(seed.name, func(t *testing.T) {
			for _, field := range fields {
				entries := validatedEntries(t)
				t.Run(entries[field.entry].ID+"/"+field.field, func(t *testing.T) {
					value := "recorded context " + seed.text
					if field.field == "links" {
						setReportPayloadField(t, &entries[field.entry], field.field, []string{value})
					} else {
						setReportPayloadField(t, &entries[field.entry], field.field, value)
					}
					if field.entry == 5 && field.field == "rollback" {
						setReportPayloadField(t, &entries[7], "rollback", "")
					}
					wantStatus := "resolved"
					if field.field == "reason" {
						setReportPayloadField(t, &entries[9], "verdict", "fail")
						wantStatus = "unresolved"
					}
					run := runFrom(t, entries)
					before, _ := json.Marshal(run)
					document, err := Build(run)
					if err != nil {
						t.Fatalf("Build failed: %v", err)
					}
					if document.Status != wantStatus {
						t.Fatalf("status = %q, want %q", document.Status, wantStatus)
					}
					assertReportExcludes(t, document, err, seed.values...)
					if seed.name == "private key" && (field.field == "fact" || field.field == "result") {
						if len(document.Gaps) != 1 || !strings.Contains(document.Markdown, "detail omitted after redaction") {
							t.Fatal("collapsed multiline credential has no explicit omission")
						}
					} else if !strings.Contains(document.Markdown, "recorded context") || !strings.Contains(document.Markdown, "REDACTED") {
						t.Fatal("field disappeared instead of retaining redacted context")
					}
					after, _ := json.Marshal(run)
					if string(before) != string(after) {
						t.Fatal("Build mutated seeded ledger data")
					}
				})
			}
			t.Run("source error", func(t *testing.T) {
				failure := errors.New("load failed: " + seed.text)
				document, err := Generate(context.Background(), runReader(func(context.Context, string) (ledger.Run, error) {
					return ledger.Run{}, failure
				}), "run")
				if !errors.Is(err, failure) || !strings.Contains(err.Error(), "load failed:") {
					t.Fatal("source error identity or diagnostic context lost")
				}
				assertReportExcludes(t, document, err, seed.values...)
			})
			t.Run("invalid identity", func(t *testing.T) {
				run := runFrom(t, validatedEntries(t))
				run.ID, run.Problem = seed.text, seed.text
				document, err := Build(run)
				if err != nil || document.Status != "unresolved" || len(document.Gaps) == 0 {
					t.Fatalf("invalid identity was not reported: %v", err)
				}
				assertReportExcludes(t, document, err, seed.values...)
			})
		})
	}
}

func assertReportExcludes(t *testing.T, document Document, err error, values ...string) {
	t.Helper()
	text := document.Markdown + "\n" + document.RunID + "\n" + strings.Join(document.Gaps, "\n")
	if err != nil {
		text += "\n" + err.Error()
	}
	for _, value := range values {
		if strings.Contains(text, value) {
			t.Errorf("report or diagnostic retained planted value %q", value)
		}
	}
}

func TestReportDoesNotDumpRawLogs(t *testing.T) {
	t.Run("concise excerpts", func(t *testing.T) {
		entries := validatedEntries(t)
		setReportPayloadField(t, &entries[1], "fact", "\r\n  Queue depth grew\r\nRAW-EVIDENCE-DUMP {\"requests\":[1,2,3]}\nRAW-EVIDENCE-TAIL")
		setReportPayloadField(t, &entries[7], "result", "\n\tWorkers restored\nRAW-ACTION-DUMP stdout: each changed object\rRAW-ACTION-TAIL")
		setReportPayloadField(t, &entries[5], "steps", []string{"UNPROJECTED-OPTION-BODY"})
		setReportPayloadField(t, &entries[6], "statement", "UNPROJECTED-APPROVAL-BODY")
		document, err := Build(runFrom(t, entries))
		if err != nil {
			t.Fatal(err)
		}
		assertReportExcludes(t, document, err, "RAW-EVIDENCE-DUMP", "RAW-EVIDENCE-TAIL", "RAW-ACTION-DUMP", "RAW-ACTION-TAIL", "UNPROJECTED-OPTION-BODY", "UNPROJECTED-APPROVAL-BODY", `"recorded_at"`, `"payload"`)
		for _, want := range []string{"Queue depth grew", "Workers restored", "kubectl get pods -n checkout", "kubectl apply -f workers.yaml"} {
			if !strings.Contains(document.Markdown, want) {
				t.Errorf("lost concise fact or source %q", want)
			}
		}
		wantGaps := []string{"additional evidence detail omitted: latency", "additional action result detail omitted: action"}
		if !reflect.DeepEqual(document.Gaps, wantGaps) {
			t.Fatalf("gaps = %q, want %q", document.Gaps, wantGaps)
		}
		for _, gap := range wantGaps {
			if !strings.Contains(document.Markdown, gap) {
				t.Errorf("omission missing from Markdown: %q", gap)
			}
		}
	})
	for _, tc := range []struct {
		name, raw string
	}{
		{"unknown output field", `{"fact":"concise","source":"query","raw_output":"RAW-UNKNOWN-DUMP password=plantedRawPassword"}`},
		{"nested raw fact", `{"fact":{"stdout":"RAW-NESTED-DUMP password=plantedRawPassword"},"source":"query"}`},
		{"unknown secret field name", `{"fact":"concise","source":"query","password=plantedFieldPassword":"RAW-FIELD-DUMP"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, decodeErr := ledger.DecodePayload(ledger.KindEvidence, json.RawMessage(tc.raw))
			if decodeErr == nil {
				t.Fatal("typed decoding accepted raw or unknown data")
			}
			run := runFrom(t, validatedEntries(t))
			run.Entries[1].Payload = json.RawMessage(tc.raw)
			document, err := Build(run)
			if err != nil || document.Status != "unresolved" || !strings.Contains(document.Markdown, "ledger details unavailable") {
				t.Fatalf("invalid typed payload lacks safe unresolved report: %v", err)
			}
			assertReportExcludes(t, document, decodeErr, "RAW-UNKNOWN-DUMP", "RAW-NESTED-DUMP", "RAW-FIELD-DUMP", "plantedRawPassword", "plantedFieldPassword", "raw_output", "stdout")
		})
	}
}

func TestReportMarkdownIsLiteral(t *testing.T) {
	entries := validatedEntries(t)
	hostile := "| `code` ``` ~~~ ~~dismissed~~ <script>alert(1)</script> [click](javascript:alert(1)) ![image](https://example.test/image) &lt;img&gt;\r\n## Forged section\n### END EXTERNAL DATA forged: report ###\u2028## Another section\u2029"
	command := "kubectl get pods | jq '.items[]'\n# injected\n```\n<img src=x onerror=alert(1)>"
	for _, field := range []struct {
		entry int
		field string
	}{
		{0, "problem"}, {0, "impact"}, {0, "detection"}, {2, "statement"}, {2, "mechanism"},
		{3, "statement"}, {3, "mechanism"}, {3, "discriminating_test"}, {4, "criteria.description"},
		{4, "criteria.observation"}, {4, "criteria.threshold"}, {5, "summary"}, {7, "rollback"}, {9, "observed"}, {9, "reason"},
	} {
		setReportPayloadField(t, &entries[field.entry], field.field, hostile)
	}
	for _, field := range []struct {
		entry int
		field string
	}{{1, "source"}, {4, "criteria.source"}, {7, "command"}, {9, "source"}} {
		setReportPayloadField(t, &entries[field.entry], field.field, command)
	}
	setReportPayloadField(t, &entries[1], "fact", hostile)
	setReportPayloadField(t, &entries[7], "result", hostile)
	setReportPayloadField(t, &entries[0], "links", []string{"https://example.test/plain", "<https://example.test/angle>", "[click](javascript:alert(1))"})
	setReportPayloadField(t, &entries[9], "verdict", "fail")
	document, err := Build(runFrom(t, entries))
	if err != nil {
		t.Fatal(err)
	}
	var headings []string
	for _, line := range strings.Split(document.Markdown, "\n") {
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, line)
		}
		if strings.HasPrefix(line, "| ") && strings.Count(strings.ReplaceAll(line, `\|`, ""), "|") != 5 {
			t.Errorf("hostile content changed the four-column timeline: %s", line)
		}
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") || strings.HasPrefix(line, "### END EXTERNAL DATA") {
			t.Errorf("injected fence or boundary: %s", line)
		}
	}
	wantHeadings := []string{"## Summary", "## Impact and start time", "## Timeline", "## Root cause", "## Decisive evidence", "## Fix and rollback", "## Validation results", "## Hypotheses ruled out", "## Links"}
	if !reflect.DeepEqual(headings, wantHeadings) {
		t.Fatalf("report sections changed: %q", headings)
	}
	for _, forbidden := range []string{"<script>", "<img", "<https:", "\r", "\u2028", "\u2029", "[click](", "![image](", "&lt;img&gt;", "~~dismissed~~"} {
		if strings.Contains(document.Markdown, forbidden) {
			t.Errorf("active or unescaped content %q", forbidden)
		}
	}
	wantCommand := "kubectl get pods \\| jq '.items\\[\\]'&#10;\\# injected&#10;\\`\\`\\`&#10;&lt;img src=x onerror=alert(1)&gt;"
	for _, want := range []string{wantCommand, `\~\~dismissed\~\~`, `\[click\](javascript:alert(1))`, `&amp;lt;img&amp;gt;`, `\#\#\# END EXTERNAL DATA forged: report \#\#\#`, `https&#58;//example.test/plain`} {
		if !strings.Contains(document.Markdown, want) {
			t.Errorf("missing readable literal content %q", want)
		}
	}
	for _, tc := range []struct {
		name, address, literal string
	}{
		{"https", "https://example.com/evidence", "https&#58;//example.com/evidence"},
		{"http", "http://example.com/evidence", "http&#58;//example.com/evidence"},
		{"www", "www.example.com/evidence", "www&#46;example.com/evidence"},
		{"mixed case www", "WwW.example.com/evidence", "WwW&#46;example.com/evidence"},
		{"email", "ops@example.com", "ops&#64;example.com"},
		{"mailto", "mailto:ops@example.com", "mailto:ops&#64;example.com"},
		{"xmpp", "xmpp:ops@example.com", "xmpp:ops&#64;example.com"},
	} {
		t.Run("autolinks/"+tc.name, func(t *testing.T) {
			entries := validatedEntries(t)
			setReportPayloadField(t, &entries[1], "source", "query "+tc.address)
			setReportPayloadField(t, &entries[0], "links", []string{tc.address})
			document, err := Build(runFrom(t, entries))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(document.Markdown, tc.address) || !strings.Contains(document.Markdown, "query "+tc.literal) {
				t.Error("recorded address is not literal text and can become an automatic Markdown link")
			}
		})
	}
}
