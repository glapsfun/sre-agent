package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/safety"
)

type RenderOptions struct {
	Offset   int
	MaxBytes int
	Compact  bool
}

type View struct {
	RunID           string   `json:"run_id"`
	ThroughSeq      int64    `json:"through_seq"`
	Text            string   `json:"text"`
	EntryIDs        []string `json:"entry_ids"`
	OmittedEntries  int      `json:"omitted_entries"`
	NextOffset      *int     `json:"next_offset"`
	TruncatedFields []string `json:"truncated_fields"`
}

// Render measures the encoded view, including escaped text and fresh boundaries.
// It never pages the snapshot used to evaluate validation.
func Render(run Run, opts RenderOptions) (View, error) {
	snapshot, err := validationSnapshot(run)
	if err != nil {
		return View{}, err
	}
	if opts.Offset < 0 || opts.Offset > len(snapshot.Entries) {
		return View{}, errors.New("offset is outside ledger history")
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 6500
	}
	if opts.MaxBytes < 2048 {
		return View{}, errors.New("ledger output budget must be at least 2048 bytes")
	}
	wrapper, err := safety.NewWrapper()
	if err != nil {
		return View{}, errors.New("cannot create ledger data boundary")
	}
	summary := EvaluateValidation(snapshot)
	count := len(snapshot.Entries) - opts.Offset
	if opts.Compact && count > 20 {
		count = 20
	}
	// Avoid building unbounded candidates even for show; pagination retains history.
	if count > 100 {
		count = 100
	}
	limit := opts.MaxBytes
	for {
		view := renderPage(snapshot, summary, opts.Offset, count, limit, wrapper)
		encoded, err := json.Marshal(view)
		if err != nil {
			return View{}, err
		}
		if len(encoded) <= opts.MaxBytes {
			return view, nil
		}
		if count > 1 {
			count /= 2
			continue
		}
		if limit > 32 {
			limit /= 2
			continue
		}
		// Collection structure can exceed the budget even after scalar clipping.
		// Identify omitted payloads without pretending entry paging recovers them.
		view = renderDiagnostic(snapshot, summary, opts.Offset, count, wrapper)
		encoded, _ = json.Marshal(view)
		if len(encoded) <= opts.MaxBytes {
			return view, nil
		}
		return View{}, errors.New("ledger output budget cannot hold paging metadata")
	}
}

type viewFields struct {
	limit     int
	truncated map[string]bool
}

func (f *viewFields) text(path, value string) string {
	value = safety.NewRedactor().Redact(value)
	if len(value) <= f.limit {
		return value
	}
	f.truncated[path] = true
	return shorten(value, f.limit)
}

func shorten(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + "... [truncated]"
}

func (f *viewFields) json(path string, value any) string {
	if value == nil {
		return "not recorded"
	}
	encoded, _ := json.Marshal(value)
	if string(encoded) == "null" || string(encoded) == "[]" || string(encoded) == "{}" {
		return "not recorded"
	}
	// Work on typed detached values so presentation cannot rewrite stored data.
	clone := f.scalarFields(path, reflect.ValueOf(value))
	encoded, _ = json.Marshal(clone.Interface())

	return string(encoded)
}

func (f *viewFields) scalarFields(path string, value reflect.Value) reflect.Value {
	if value.Type() == reflect.TypeFor[time.Time]() {
		return value
	}
	result := reflect.New(value.Type()).Elem()
	switch value.Kind() {
	case reflect.String:
		result.SetString(f.text(path, value.String()))
	case reflect.Pointer:
		if !value.IsNil() {
			result.Set(reflect.New(value.Type().Elem()))
			result.Elem().Set(f.scalarFields(path, value.Elem()))
		}
	case reflect.Struct:
		for i := range value.NumField() {
			name := strings.Split(value.Type().Field(i).Tag.Get("json"), ",")[0]
			result.Field(i).Set(f.scalarFields(path+"."+name, value.Field(i)))
		}
	case reflect.Slice:
		if !value.IsNil() {
			result.Set(reflect.MakeSlice(value.Type(), value.Len(), value.Len()))
			for i := range value.Len() {
				result.Index(i).Set(f.scalarFields(fmt.Sprintf("%s[%d]", path, i), value.Index(i)))
			}
		}
	case reflect.Map:
		// Map keys need shared key-aware redaction; they are not ledger identities.
		raw, _ := json.Marshal(value.Interface())
		_ = json.Unmarshal([]byte(safety.NewRedactor().Redact(string(raw))), result.Addr().Interface())
		keys := result.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for i, key := range keys {
			// Indexed paths avoid exposing untrusted coordinate keys outside the wrapper.
			result.SetMapIndex(key, f.scalarFields(fmt.Sprintf("%s[%d].value", path, i), result.MapIndex(key)))
		}
	default:
		result.Set(value)
	}
	return result
}

func historyCount(entries []Entry) string {
	if len(entries) == 0 {
		return "not recorded"
	}
	return fmt.Sprintf("%d recorded (see history)", len(entries))
}

func renderPage(run Run, summary ValidationSummary, offset, count, limit int, wrapper *safety.Wrapper) View {
	fields := viewFields{limit: limit, truncated: map[string]bool{}}
	var out strings.Builder
	fmt.Fprintf(&out, "INVESTIGATION LEDGER - %s\n", fields.text("problem", run.Problem))
	phase := "not recorded"
	for _, entry := range run.Entries {
		if entry.Kind == KindMetadata {
			payload, _ := DecodePayload(entry.Kind, entry.Payload)
			if payload.(MetadataUpdate).Phase != nil {
				phase = fmt.Sprint(run.State.Phase)
			}
		}
	}
	fmt.Fprintf(&out, "Phase: %s\nEnvironment: %s\nTools: %s | Missing: %s\nWave 1 investigators: %s\nWave 2 investigators: %s\n", phase, fields.json("environment", run.State.Environment), fields.json("tools.available", run.State.Tools.Available), fields.json("tools.missing", run.State.Tools.Missing), fields.json("wave1", run.State.Wave1), fields.json("wave2", run.State.Wave2))
	expected := "not recorded"
	if run.State.Expected != nil {
		payload, _ := DecodePayload(KindExpectedBehavior, run.State.Expected.Payload)
		expected = fields.json("expected", payload)
	}
	options := historyCount(run.State.Options)
	if len(run.State.Approvals) > 0 {
		options += "; approvals: " + historyCount(run.State.Approvals)
	}
	fmt.Fprintf(&out, "Evidence: %s\nHypotheses: %s\nExpected behavior: %s\nOptions proposed: %s\nActions taken: %s\nValidation: %s\n", historyCount(run.State.Evidence), historyCount(run.State.Hypotheses), expected, options, historyCount(run.State.Actions), summary.Verdict)
	if len(summary.Criteria) > 0 {
		fmt.Fprintf(&out, "Criterion results: %s\n", fields.json("validation.criteria", summary.Criteria))
	}
	if len(run.State.SafetyViolations) > 0 {
		fmt.Fprintf(&out, "SAFETY VIOLATION: %s\n", historyCount(run.State.SafetyViolations))
	} else {
		out.WriteString("Safety violations: not recorded\n")
	}
	if run.State.Impact != "" {
		fmt.Fprintf(&out, "Impact: %s\n", fields.text("impact", run.State.Impact))
	}
	if run.State.StartedAt != nil {
		fmt.Fprintf(&out, "Started at: %s\n", run.State.StartedAt.Format(time.RFC3339Nano))
	}
	if run.State.Detection != "" {
		fmt.Fprintf(&out, "Detection: %s\n", fields.text("detection", run.State.Detection))
	}
	if len(run.State.Links) > 0 {
		fmt.Fprintf(&out, "Links: %s\n", fields.json("links", run.State.Links))
	}
	out.WriteString("History:\n")
	ids := make([]string, 0, count)
	for _, entry := range run.Entries[offset : offset+count] {
		payload, _ := DecodePayload(entry.Kind, entry.Payload)
		label := string(entry.Kind)
		if entry.Kind == KindSafetyViolation {
			label = "SAFETY VIOLATION"
		}
		fmt.Fprintf(&out, "[%s] seq=%d %s at=%s %s\n", entry.ID, entry.Seq, label, entry.RecordedAt.Format(time.RFC3339Nano), fields.json("entries."+entry.ID+".payload", payload))
		ids = append(ids, entry.ID)
	}
	return finishView(run, offset, ids, out.String(), fields.truncated, wrapper)
}

func finishView(run Run, offset int, ids []string, text string, truncated map[string]bool, wrapper *safety.Wrapper) View {
	view := View{RunID: run.ID, ThroughSeq: run.ThroughSeq, EntryIDs: ids, OmittedEntries: len(run.Entries) - len(ids), TruncatedFields: []string{}}
	for field := range truncated {
		view.TruncatedFields = append(view.TruncatedFields, field)
	}
	sort.Strings(view.TruncatedFields)
	text += fmt.Sprintf("Omitted entries: %d\n", view.OmittedEntries)
	if offset+len(ids) < len(run.Entries) {
		next := offset + len(ids)
		view.NextOffset = &next
		text += fmt.Sprintf("Next page: sre run show %s --offset %d\n", run.ID, next)
	}
	if len(view.TruncatedFields) > 0 {
		text += "Truncated fields: " + strings.Join(view.TruncatedFields, ", ") + "\n"
		text += "Displayed fields shortened or omitted; stored entries are unchanged. Entry offsets page history, not content within a field.\n"
	}
	view.Text = wrapper.Wrap("investigation ledger", strings.TrimSuffix(text, "\n"))
	return view
}

func renderDiagnostic(run Run, summary ValidationSummary, offset, count int, wrapper *safety.Wrapper) View {
	const omitted = "omitted: ledger content exceeds output budget"
	text := "INVESTIGATION LEDGER - " + omitted + "\nPhase: " + omitted + "\nEnvironment: " + omitted + "\nTools: " + omitted + " | Missing: " + omitted + "\nWave 1 investigators: " + omitted + "\nWave 2 investigators: " + omitted + "\nEvidence: " + historyCount(run.State.Evidence) + "\nHypotheses: " + historyCount(run.State.Hypotheses) + "\nExpected behavior: " + omitted + "\nOptions proposed: " + historyCount(run.State.Options) + "\nActions taken: " + historyCount(run.State.Actions) + "\nValidation: " + summary.Verdict + "\nSAFETY VIOLATION records: " + historyCount(run.State.SafetyViolations) + "\nHistory:\n"
	ids := make([]string, 0, count)
	fields := map[string]bool{"problem": true, "phase": true, "environment": true, "tools": true, "wave1": true, "wave2": true, "expected": true, "validation.criteria": true}
	if run.State.Impact != "" {
		text += "Impact: " + omitted + "\n"
		fields["impact"] = true
	}
	if run.State.StartedAt != nil {
		text += "Started at: " + run.State.StartedAt.Format(time.RFC3339Nano) + "\n"
	}
	if run.State.Detection != "" {
		text += "Detection: " + omitted + "\n"
		fields["detection"] = true
	}
	if len(run.State.Links) > 0 {
		text += "Links: " + omitted + "\n"
		fields["links"] = true
	}
	for _, entry := range run.Entries[offset : offset+count] {
		ids = append(ids, entry.ID)
		text += fmt.Sprintf("[%s] seq=%d %s: identity only; payload omitted (output budget)\n", entry.ID, entry.Seq, entry.Kind)
		fields["entries."+entry.ID+".payload"] = true
	}
	text += fmt.Sprintf("Omitted payloads on this page: %d\n", len(ids))
	text += "Entry pagination cannot recover this oversized content through the bounded CLI; full records remain in the store/service.\n"
	return finishView(run, offset, ids, text, fields, wrapper)
}
