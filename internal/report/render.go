package report

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
)

func render(document Document, model projection) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\nRun: %s\nThrough sequence: %d\nStatus: %s\n", literal(model.problem), literal(document.RunID), document.ThroughSeq, document.Status)
	section(&out, "Summary")
	field(&out, "Problem", model.problem)
	field(&out, "Validation outcome", model.summary.Verdict)
	for _, reason := range model.summary.Reasons {
		field(&out, "Unresolved reason", reason)
	}
	for _, gap := range document.Gaps {
		field(&out, "Gap", gap)
	}
	section(&out, "Impact and start time")
	field(&out, "Impact", model.impact)
	field(&out, "Start time", timestamp(model.startedAt))
	field(&out, "Detection", model.detection)
	section(&out, "Timeline")
	renderTimeline(&out, model)
	section(&out, "Root cause")
	if len(model.causes) == 0 {
		out.WriteString("Root cause not confirmed.\n")
	} else {
		if len(model.causes) > 1 {
			out.WriteString("Multiple confirmed hypotheses recorded.\n\n")
		}
		for _, cause := range model.causes {
			fmt.Fprintf(&out, "- %s: %s\n", literal(cause.id), literal(cause.value.Statement))
			field(&out, "  Mechanism", cause.value.Mechanism)
			field(&out, "  Supporting evidence", strings.Join(cause.value.Supporting, ", "))
		}
	}
	section(&out, "Decisive evidence")
	renderEvidence(&out, model)
	section(&out, "Fix and rollback")
	fix := ""
	if document.Status == "resolved" {
		fix = model.summary.ActionID
	}
	field(&out, "Validated fix", fix)
	field(&out, "Selected action", model.summary.ActionID)
	if len(model.actions) == 0 {
		out.WriteString("\nActions: not recorded\n")
	}
	for i, action := range model.actions {
		if i == 0 {
			out.WriteByte('\n')
		}
		value := action.value
		fmt.Fprintf(&out, "- %s: %s (%s)\n", literal(action.id), literal(value.Effect), literal(value.Status))
		field(&out, "  Command", value.Command)
		field(&out, "  Result", value.Result)
		option, exists := model.options[value.OptionID]
		if exists {
			fmt.Fprintf(&out, "  Option: %s; %s\n", literal(value.OptionID), literal(option.Summary))
		} else {
			field(&out, "  Option", value.OptionID)
		}
		field(&out, "  Approval", value.ApprovalID)
		rollback := value.Rollback
		if strings.TrimSpace(rollback) == "" {
			rollback = option.Rollback
		}
		field(&out, "  Rollback", rollback)
	}
	section(&out, "Validation results")
	renderValidation(&out, model)
	section(&out, "Hypotheses ruled out")
	if len(model.rejected) == 0 {
		out.WriteString("not recorded\n")
	}
	for _, hypothesis := range model.rejected {
		value := hypothesis.value
		fmt.Fprintf(&out, "- %s (%s): %s\n", literal(hypothesis.id), literal(value.Status), literal(value.Statement))
		field(&out, "  Mechanism", value.Mechanism)
		field(&out, "  Contradicting evidence", strings.Join(value.Contradicting, ", "))
		field(&out, "  Discriminating test", value.DiscriminatingTest)
	}
	section(&out, "Links")
	seen := map[string]bool{}
	for _, link := range model.links {
		link = strings.TrimSpace(safety.NewRedactor().Redact(link))
		if link == "" || seen[link] {
			continue
		}
		seen[link] = true
		// Encode colons too: link text must not become an automatic GFM link.
		fmt.Fprintf(&out, "- %s\n", strings.ReplaceAll(literal(link), ":", "&#58;"))
	}
	if len(seen) == 0 {
		out.WriteString("not recorded\n")
	}
	return out.String()
}

type event struct {
	at                      time.Time
	seq                     int64
	id, description, source string
}

func renderTimeline(out *strings.Builder, model projection) {
	var events []event
	var undated []record[ledger.Evidence]
	for _, evidence := range model.evidence {
		if evidence.value.ObservedAt == nil {
			undated = append(undated, evidence)
			continue
		}
		events = append(events, event{*evidence.value.ObservedAt, evidence.seq, evidence.id, "Evidence: " + nonempty(evidence.value.Fact), evidence.value.Source})
	}
	for _, action := range model.actions {
		value := action.value
		events = append(events, event{value.At, action.seq, action.id, value.Effect + " (" + value.Status + "): " + nonempty(value.Result), value.Command})
	}
	for _, validation := range model.results {
		value := validation.value
		events = append(events, event{value.At, validation.seq, validation.id, "Validation " + value.CriterionID + " (" + value.Verdict + "): " + nonempty(value.Observed), value.Source})
	}
	sort.Slice(events, func(i, j int) bool {
		if !events[i].at.Equal(events[j].at) {
			return events[i].at.Before(events[j].at)
		}
		if events[i].seq != events[j].seq {
			return events[i].seq < events[j].seq
		}
		return events[i].id < events[j].id
	})
	if len(events) != 0 {
		out.WriteString("| Time (UTC) | Entry | Event | Source |\n| :--- | :--- | :--- | :--- |\n")
		for _, item := range events {
			fmt.Fprintf(out, "| %s | %s | %s | %s |\n", timestamp(&item.at), literal(item.id), literal(item.description), literal(item.source))
		}
	} else {
		out.WriteString("Dated events: not recorded\n")
	}
	if len(undated) != 0 {
		out.WriteString("\nUndated evidence:\n\n")
		for _, evidence := range undated {
			fmt.Fprintf(out, "- %s: %s\n", literal(evidence.id), literal(evidence.value.Fact))
			field(out, "  Source", evidence.value.Source)
		}
	}
}

func renderEvidence(out *strings.Builder, model projection) {
	cited := map[string]bool{}
	for _, cause := range model.causes {
		for _, id := range cause.value.Supporting {
			cited[id] = true
		}
	}
	for _, action := range model.actions {
		if action.id == model.summary.ActionID {
			for _, id := range action.value.EvidenceIDs {
				cited[id] = true
			}
		}
	}
	for _, criterion := range model.summary.Criteria {
		for _, id := range model.validations[criterion.ValidationID].EvidenceIDs {
			cited[id] = true
		}
	}
	count := 0
	for _, evidence := range model.evidence {
		if !cited[evidence.id] {
			continue
		}
		count++
		fmt.Fprintf(out, "- %s: %s\n", literal(evidence.id), literal(evidence.value.Fact))
		field(out, "  Source", evidence.value.Source)
	}
	if count == 0 {
		out.WriteString("No evidence cited by confirmed hypotheses, the selected action or current validation.\n")
	}
}

func renderValidation(out *strings.Builder, model projection) {
	field(out, "Expected behavior", model.expectedID)
	results := make(map[string]ledger.CriterionResult, len(model.summary.Criteria))
	for _, result := range model.summary.Criteria {
		results[result.CriterionID] = result
	}
	for i, criterion := range model.expected.Criteria {
		if i == 0 {
			out.WriteByte('\n')
		}
		result, exists := results[criterion.ID]
		if !exists {
			result = ledger.CriterionResult{Verdict: "unknown", Reason: "no current validation recorded"}
		}
		validation := model.validations[result.ValidationID]
		fmt.Fprintf(out, "- %s: %s\n", literal(criterion.ID), literal(criterion.Description))
		field(out, "  Observation", criterion.Observation)
		field(out, "  Threshold", criterion.Threshold)
		field(out, "  Criterion source", criterion.Source)
		window := ""
		if criterion.WindowSeconds != nil {
			window = fmt.Sprintf("%d seconds", *criterion.WindowSeconds)
		}
		field(out, "  Declared window", window)
		field(out, "  Observed", result.Observed)
		field(out, "  Verdict", result.Verdict)
		field(out, "  Validation", result.ValidationID)
		field(out, "  Source", validation.Source)
		field(out, "  Observed at", timestamp(&validation.At))
		field(out, "  Window start", timestamp(validation.WindowStart))
		field(out, "  Window end", timestamp(validation.WindowEnd))
		field(out, "  Reason", result.Reason)
	}
}

func section(out *strings.Builder, heading string) { fmt.Fprintf(out, "\n## %s\n\n", heading) }
func field(out *strings.Builder, label, value string) {
	fmt.Fprintf(out, "%s: %s\n", label, literal(value))
}
func nonempty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "not recorded"
	}
	return value
}
func timestamp(at *time.Time) string {
	if at == nil || at.IsZero() {
		return "not recorded"
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func literal(value string) string {
	value = nonempty(safety.NewRedactor().Redact(value))
	var out strings.Builder
	for _, r := range value {
		switch r {
		case '&':
			out.WriteString("&amp;")
		case '<':
			out.WriteString("&lt;")
		case '>':
			out.WriteString("&gt;")
		case '\\', '`', '*', '_', '[', ']', '#', '|':
			out.WriteByte('\\')
			out.WriteRune(r)
		default:
			if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
				fmt.Fprintf(&out, "&#%d;", r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}
