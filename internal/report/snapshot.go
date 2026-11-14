package report

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/glapsfun/sre-agent/internal/ledger"
)

type SnapshotOptions struct {
	Stage  string
	Detail bool
	Rules  []Rule
	Now    func() time.Time
}

type Snapshot struct {
	Document
	Stage       string    `json:"stage"`
	GeneratedAt time.Time `json:"generated_at"`
	Draft       bool      `json:"draft"`
	Detail      string    `json:"detail,omitempty"`
	Flags       []string  `json:"flags"`
}

func GenerateSnapshot(ctx context.Context, source RunReader, runID string, opts SnapshotOptions) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if opts.Stage == "" {
		opts.Stage = "interim"
	}
	if opts.Stage != "interim" && opts.Stage != "post-incident" {
		return Snapshot{}, errors.New("report stage requires interim or post-incident")
	}
	p, err := newPolicy(opts.Rules)
	if err != nil {
		return Snapshot{}, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	at := opts.Now().UTC()
	if at.IsZero() || at.Year() < 0 || at.Year() > 9999 {
		return Snapshot{}, errors.New("invalid report generation time")
	}
	if strings.TrimSpace(runID) == "" {
		return Snapshot{}, errors.New("report requires a run identifier")
	}
	run, err := source.Load(ctx, runID)
	if err != nil {
		return Snapshot{}, sourceError{err}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if run.ID != runID {
		return Snapshot{}, errors.New("report source returned a different run")
	}
	snapshot, err := ledger.Reduce(run.ID, run.Problem, run.CreatedAt, run.Entries)
	if err != nil {
		return Snapshot{}, ledger.ErrCorrupt
	}
	validation := ledger.EvaluateValidation(snapshot)
	doc := Snapshot{Document: Document{RunID: p.clean(run.ID), ThroughSeq: snapshot.ThroughSeq, Status: "unresolved", Gaps: []string{}}, Stage: opts.Stage, GeneratedAt: at, Draft: true, Flags: []string{}}
	if validation.EligibleForCapture && validation.Verdict == "resolved" {
		doc.Status = "resolved"
	}
	for _, reason := range validation.Reasons {
		doc.Gaps = append(doc.Gaps, p.clean(reason))
	}
	if len(snapshot.State.Evidence) == 0 {
		doc.Gaps = append(doc.Gaps, "Evidence not recorded")
	}
	if len(snapshot.State.Hypotheses) == 0 {
		doc.Gaps = append(doc.Gaps, "Hypotheses not recorded")
	}
	for _, entry := range snapshot.State.Evidence {
		payload, _ := ledger.DecodePayload(entry.Kind, entry.Payload)
		if gap := payload.(ledger.Evidence).Gap; gap != "" {
			doc.Gaps = append(doc.Gaps, entry.ID+": "+gap)
		}
	}
	w := snapshotWriter{policy: p}
	w.header(snapshot, doc, "Summary")
	w.field("Impact", snapshot.State.Impact)
	w.field("Service/workload", snapshot.State.Environment.Workload)
	w.field("Environment/context", snapshot.State.Environment.Context)
	w.field("Namespace", snapshot.State.Environment.Namespace)
	w.field("Detection", snapshot.State.Detection)
	w.field("Validation outcome", validation.Verdict)
	w.field("Selected action", validation.ActionID)
	if doc.Status == "resolved" {
		w.field("Validated fix", validation.ActionID)
	}
	w.investigation(snapshot.State.Investigation)
	w.section("Findings and hypotheses")
	if len(snapshot.State.Hypotheses) == 0 {
		w.field("Hypotheses", "")
	}
	for _, entry := range snapshot.State.Hypotheses {
		payload, _ := ledger.DecodePayload(entry.Kind, entry.Payload)
		value := payload.(ledger.Hypothesis)
		w.field("Hypothesis "+entry.ID+" ("+value.Status+")", value.Statement)
		w.field("Mechanism", value.Mechanism)
		w.field("Confidence", value.Confidence)
		w.field("Supporting evidence", strings.Join(value.Supporting, ", "))
		w.field("Contradicting evidence", strings.Join(value.Contradicting, ", "))
		w.field("Next check", value.DiscriminatingTest)
	}
	w.section("Evidence and sources")
	for i, entry := range snapshot.State.Evidence {
		if i == 8 {
			doc.Gaps = append(doc.Gaps, fmt.Sprintf("%d additional evidence entries omitted from summary; request detail", len(snapshot.State.Evidence)-i))
			break
		}
		payload, _ := ledger.DecodePayload(entry.Kind, entry.Payload)
		value := payload.(ledger.Evidence)
		fact := summarize(p.clean(value.Fact), "additional evidence detail omitted from summary: "+entry.ID, &doc.Gaps)
		w.field(entry.ID, fact)
		w.field("Observed at", timestamp(value.ObservedAt))
		w.field("Source", value.Source)
		for _, link := range value.Links {
			w.link(link)
		}
	}
	if len(snapshot.State.Evidence) == 0 {
		w.field("Evidence", "")
	}
	for _, link := range snapshot.State.Links {
		w.link(link)
	}
	w.section("Actions, possible resolutions and risks")
	for _, entry := range snapshot.State.Options {
		payload, _ := ledger.DecodePayload(entry.Kind, entry.Payload)
		option := payload.(ledger.Option)
		w.field("Proposed option "+entry.ID, option.Summary)
		w.field("Risk", option.Risk)
		w.field("Expected impact", option.ExpectedImpact)
		w.field("Rollback", option.Rollback)
	}
	for _, entry := range snapshot.State.Actions {
		payload, _ := ledger.DecodePayload(entry.Kind, entry.Payload)
		action := payload.(ledger.Action)
		w.field("Recorded action "+entry.ID+" ("+action.Status+")", action.Result)
		w.field("Approval reference", action.ApprovalID)
	}
	if len(snapshot.State.Actions) == 0 {
		w.field("Actions", "")
	}
	for _, criterion := range validation.Criteria {
		w.field("Validation "+criterion.CriterionID+" ("+criterion.Verdict+")", criterion.Observed+" "+criterion.Reason)
	}
	w.section("Gaps and next steps")
	for i, gap := range doc.Gaps {
		doc.Gaps[i] = p.clean(gap)
		w.field("Gap", doc.Gaps[i])
	}
	if len(doc.Gaps) == 0 {
		w.out.WriteString("No additional gaps identified in the recorded snapshot.\n")
	}
	w.out.WriteString("Review unresolved hypotheses and evidence gaps before acting. External sources were not fetched; links may become unavailable.\n")
	doc.Markdown = w.out.String()
	if opts.Detail {
		detail := snapshotWriter{policy: p}
		detail.header(snapshot, doc, "Detailed timeline")
		detail.investigation(snapshot.State.Investigation)
		detail.out.WriteString("Entries are ordered by ledger sequence. Recorded time is not necessarily the event time.\n")
		for _, entry := range snapshot.Entries {
			payload, _ := ledger.DecodePayload(entry.Kind, entry.Payload)
			fmt.Fprintf(&detail.out, "\n### %d. %s (%s)\n\n", entry.Seq, detail.text(entry.ID), detail.text(string(entry.Kind)))
			detail.field("Recorded at", entry.RecordedAt.UTC().Format(time.RFC3339Nano))
			detail.value("", reflect.ValueOf(payload))
		}
		if len(snapshot.Entries) == 0 {
			detail.field("Entries", "")
		}
		detail.section("Gaps")
		for _, gap := range doc.Gaps {
			detail.field("Gap", gap)
		}
		detail.out.WriteString("External sources were not fetched; links may become unavailable. This draft remains readable without the investigation database.\n")
		doc.Detail = detail.out.String()
	}
	doc.Flags = p.flagged()
	doc.Markdown += p.review()
	if opts.Detail {
		doc.Detail += p.review()
	}
	return doc, nil
}

func (w *snapshotWriter) investigation(state *ledger.InvestigationState) {
	if state == nil {
		w.field("Investigation rounds", "")
		return
	}
	w.section("Investigation rounds")
	w.field("Round (initial 0; at most 4 additional)", fmt.Sprint(state.Number))
	w.field("Collection active", fmt.Sprint(state.Active))
	w.field("Recorded assessment", state.Assessment)
	w.field("Stop reason", state.StopReason)
	w.field("Reason", state.Reason)
	for _, check := range state.Checks {
		w.field("Check", check)
	}
	w.field("Round evidence", strings.Join(state.EvidenceIDs, ", "))
	for _, question := range state.Questions {
		w.field("Unresolved question", question)
	}
}

type snapshotWriter struct {
	out    strings.Builder
	policy *policy
}

func (w *snapshotWriter) text(s string) string { return literal(w.policy.clean(s)) }
func (w *snapshotWriter) field(name, value string) {
	fmt.Fprintf(&w.out, "%s: %s\n\n", w.text(name), w.text(value))
}
func (w *snapshotWriter) section(name string) { fmt.Fprintf(&w.out, "\n## %s\n\n", name) }
func (w *snapshotWriter) header(run ledger.Run, doc Snapshot, heading string) {
	fmt.Fprintf(&w.out, "# %s\n\nDraft: human review is required before sharing or committing. Automated redaction is not complete.\n\n", w.text(run.Problem))
	w.field("Run", run.ID)
	w.field("Through sequence", fmt.Sprint(doc.ThroughSeq))
	w.field("Generated at", doc.GeneratedAt.Format(time.RFC3339Nano))
	w.field("Stage", doc.Stage)
	w.field("Status", doc.Status)
	w.section(heading)
}

func (w *snapshotWriter) link(raw string) {
	canonical := raw
	u, err := url.Parse(raw)
	if err == nil && u.RawQuery != "" {
		query, queryErr := url.ParseQuery(u.RawQuery)
		if queryErr != nil {
			w.field("Source link", "unavailable: malformed URL query")
			return
		}
		u.RawQuery = query.Encode()
		canonical = u.String()
	}
	clean := w.policy.clean(canonical)
	if err != nil || u == nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || clean != canonical || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		w.field("Source link (inactive)", clean)
		return
	}
	target := strings.NewReplacer("(", "%28", ")", "%29", "<", "%3C", ">", "%3E", "\"", "%22", "\\", "%5C", " ", "%20").Replace(canonical)
	fmt.Fprintf(&w.out, "Source link: [%s](%s)\n\n", w.text(clean), target)
}

// Render only decoded payload values, preserving field names and types without
// embedding raw JSON or interpreting recorded commands as executable content.
func (w *snapshotWriter) value(label string, v reflect.Value) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			w.field(label, "")
			return
		}
		w.value(label, v.Elem())
		return
	}
	if at, ok := v.Interface().(time.Time); ok {
		w.field(label, at.UTC().Format(time.RFC3339Nano))
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			name := strings.Split(v.Type().Field(i).Tag.Get("json"), ",")[0]
			if name == "" {
				name = v.Type().Field(i).Name
			}
			w.value(strings.TrimSpace(label+" "+strings.ReplaceAll(name, "_", " ")), v.Field(i))
		}
	case reflect.Slice:
		if v.Len() == 0 {
			w.field(label, "")
		}
		for i := 0; i < v.Len(); i++ {
			if strings.HasSuffix(label, "links") && v.Index(i).Kind() == reflect.String {
				w.link(v.Index(i).String())
			} else {
				w.value(label, v.Index(i))
			}
		}
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		if len(keys) == 0 {
			w.field(label, "")
		}
		for _, key := range keys {
			w.value(label+" "+key.String(), v.MapIndex(key))
		}
	default:
		w.field(label, fmt.Sprint(v.Interface()))
	}
}
