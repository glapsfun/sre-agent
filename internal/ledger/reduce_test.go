package ledger

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var ledgerTime = time.Date(2026, 11, 12, 1, 0, 0, 123, time.UTC)

func ledgerFixtures() []Entry {
	payloads := []struct {
		id, kind, raw string
	}{
		{"m1", "metadata", `{"problem":"checkout errors","phase":6,"environment":{"context":"prod","namespace":"shop","workload":"checkout","gitops_manager":"flux","cloud":"aws","source_repos":[{"path":"/src","remote":"example/source"}],"gitops_repos":[{"path":"/config","remote":"example/config"}],"cloud_coordinates":{"region":"us-east-1","account":"example-account"},"notes":["memo verified","old endpoint needs verification"]},"tools":{"available":["kubectl"],"missing":[{"name":"metrics","blocks":"cannot verify latency"}]},"wave1":{"investigators":["logs","k8s"],"reason":"first evidence","mutation_check":"verified"},"wave2":{"investigators":[],"reason":"high confidence reached","mutation_check":"not_run"},"impact":"checkout unavailable","started_at":"2026-11-12T00:00:00Z","detection":"alert","links":["https://example/incident"],"session_id":"session-1"}`},
		{"e1", "evidence", `{"fact":"errors rose","source":"sum(rate(errors[5m]))","observed_at":"2026-11-12T00:05:00Z","links":["https://example/graph"]}`},
		{"e2", "evidence", `{"fact":"one pod healthy","source":"kubectl get pods"}`},
		{"h1", "hypothesis", `{"statement":"bad rollout","mechanism":"new image rejects traffic","confidence":"high","supporting":["e1"],"contradicting":["e2"],"discriminating_test":"compare image hashes","status":"confirmed"}`},
		{"x1", "expected-behavior", `{"criteria":[{"id":"errors","description":"error rate below one percent","observation":"rate(errors)","threshold":"< 1%","window_seconds":900,"source":"metrics"},{"id":"ready","description":"all pods ready","observation":"pod readiness","threshold":"","source":""}],"reason":"agreed success conditions"}`},
		{"o1", "option", `{"summary":"roll back image","steps":["revert image"],"risk":"old behavior returns","pros":["known image"],"cons":["feature unavailable"],"expected_impact":"restore checkout","rollback":"restore current image","hypothesis_ids":["h1"]}`},
		{"p1", "approval", `{"option_id":"o1","decision":"approved","statement":"apply option one","at":"2026-11-12T00:10:00Z","target":"prod"}`},
		{"a1", "action", `{"at":"2026-11-12T00:11:00Z","command":"revert image","result":"rollout complete","effect":"remediation","status":"applied","option_id":"o1","approval_id":"p1","evidence_ids":["e1"],"rollback":"restore current image"}`},
		{"v1", "validation", `{"expected_entry_id":"x1","criterion_id":"errors","action_id":"a1","verdict":"pass","observed":"0.1%","source":"rate(errors)","evidence_ids":["e1"],"at":"2026-11-12T01:00:00.000000123Z","window_start":"2026-11-12T00:12:00Z","window_end":"2026-11-12T00:27:00Z","reason":"stable throughout"}`},
		{"s1", "safety-violation", `{"wave":1,"investigators":["logs"],"changes":[{"object":"Deployment/checkout","field":"spec.replicas","before_hash":"before","after_hash":"after","change":"changed"},{"object":"HEAD","field":"","before_hash":"old-revision","after_hash":"new-revision","change":"head"}]}`},
		{"s2", "safety-violation", `{"wave":1,"investigators":[],"changes":[],"acknowledges":"s1","acknowledgment":"reviewed unexpected change"}`},
	}
	entries := make([]Entry, len(payloads))
	for i, p := range payloads {
		entries[i] = Entry{ID: p.id, RunID: "run-1", Seq: int64(i + 1), Kind: Kind(p.kind), RecordedAt: ledgerTime.Add(time.Duration(i) * time.Second), Payload: json.RawMessage(p.raw)}
	}
	return entries
}

func decoded[T any](t *testing.T, entry Entry) T {
	t.Helper()
	value, err := DecodePayload(entry.Kind, entry.Payload)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := value.(T)
	if !ok {
		t.Fatalf("decoded %T, want %T", value, result)
	}
	return result
}

func TestReduceEveryLedgerField(t *testing.T) {
	entries := ledgerFixtures()
	run, err := Reduce("run-1", "original problem", ledgerTime, entries)
	if err != nil {
		t.Fatal(err)
	}
	metadata := decoded[MetadataUpdate](t, entries[0])
	state := run.State
	if run.ID != "run-1" || run.Problem != *metadata.Problem || run.CreatedAt != ledgerTime || run.ThroughSeq != 11 || state.Phase != 6 {
		t.Fatalf("run metadata lost: %+v", run)
	}
	if !reflect.DeepEqual(state.Environment, *metadata.Environment) || !reflect.DeepEqual(state.Tools, *metadata.Tools) || !reflect.DeepEqual(state.Wave1, *metadata.Wave1) || !reflect.DeepEqual(state.Wave2, *metadata.Wave2) {
		t.Fatalf("environment, tools or waves lost: %+v", state)
	}
	if state.Impact != *metadata.Impact || state.Detection != *metadata.Detection || !state.StartedAt.Equal(*metadata.StartedAt) || !reflect.DeepEqual(state.Links, *metadata.Links) {
		t.Fatalf("incident metadata lost: %+v", state)
	}
	groups := []struct {
		got  []Entry
		want []Entry
	}{{state.Evidence, entries[1:3]}, {state.Hypotheses, entries[3:4]}, {state.Options, entries[5:6]}, {state.Approvals, entries[6:7]}, {state.Actions, entries[7:8]}, {state.Validations, entries[8:9]}, {state.SafetyViolations, entries[9:11]}}
	for _, group := range groups {
		if !reflect.DeepEqual(group.got, group.want) {
			t.Fatalf("entry group lost: got %+v want %+v", group.got, group.want)
		}
	}
	if state.Expected == nil || !reflect.DeepEqual(*state.Expected, entries[4]) || !reflect.DeepEqual(run.Entries, entries) {
		t.Fatal("expected criteria or complete history lost")
	}
	if got := decoded[Hypothesis](t, state.Hypotheses[0]); !reflect.DeepEqual(got.Contradicting, []string{"e2"}) || got.Mechanism != "new image rejects traffic" {
		t.Fatalf("hypothesis lost fields: %+v", got)
	}
	if got := decoded[ExpectedBehavior](t, *state.Expected); len(got.Criteria) != 2 || *got.Criteria[0].WindowSeconds != 900 || got.Criteria[1].Threshold != "" {
		t.Fatalf("criteria changed: %+v", got)
	}
	if got := decoded[SafetyViolation](t, state.SafetyViolations[0]); got.Changes[1].BeforeHash != "old-revision" || got.Changes[1].AfterHash != "new-revision" || got.Changes[0].Field != "spec.replicas" {
		t.Fatalf("safety hashes lost: %+v", got)
	}
	if got := decoded[Validation](t, state.Validations[0]); got.WindowStart == nil || got.WindowEnd == nil || got.Reason != "stable throughout" {
		t.Fatalf("validation window lost: %+v", got)
	}
	if got := decoded[Action](t, state.Actions[0]); got.ApprovalID != "p1" || got.Rollback != "restore current image" {
		t.Fatalf("action approval/rollback lost: %+v", got)
	}
}

func entryWith(t *testing.T, id string, seq int64, kind Kind, payload any) Entry {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return Entry{ID: id, RunID: "run-1", Seq: seq, Kind: kind, RecordedAt: ledgerTime, Payload: raw}
}

func TestMetadataPatchPreservesOtherFields(t *testing.T) {
	entries := ledgerFixtures()[:1]
	entries = append(entries,
		entryWith(t, "m2", 2, "metadata", map[string]any{"impact": "degraded"}),
		entryWith(t, "m3", 3, "metadata", map[string]any{"phase": 0, "links": []string{}}),
		entryWith(t, "m4", 4, "metadata", map[string]any{"impact": "restored"}),
	)
	run, err := Reduce("run-1", "initial", ledgerTime, entries)
	if err != nil {
		t.Fatal(err)
	}
	if run.State.Impact != "restored" || run.State.Phase != 0 || len(run.State.Links) != 0 || run.State.Environment.Context != "prod" || run.State.Detection != "alert" || run.Problem != "checkout errors" {
		t.Fatalf("patch overwrote omitted metadata: %+v", run)
	}
	unknown := Environment{SourceRepos: []Repository{}, GitOpsRepos: []Repository{}, CloudCoordinates: map[string]string{}, Notes: []string{}}
	entries = append(entries, entryWith(t, "m5", 5, "metadata", MetadataUpdate{Environment: &unknown}))
	run, err = Reduce("run-1", "initial", ledgerTime, entries)
	if err != nil || !reflect.DeepEqual(run.State.Environment, Environment{}) || run.State.Tools.Missing[0].Blocks != "cannot verify latency" {
		t.Fatalf("explicit environment replacement failed: %+v, %v", run, err)
	}
}

func TestHypothesisSupersessionPreservesHistory(t *testing.T) {
	fixtures := ledgerFixtures()
	entries := fixtures[1:4]
	h := decoded[Hypothesis](t, entries[2])
	h.Supersedes, h.Status, h.Confidence = "h1", "ruled_out", "low"
	entries = append(entries, entryWith(t, "h2", 5, "hypothesis", h))
	h.Supersedes, h.Statement = "", "independent cause"
	entries = append(entries, entryWith(t, "h3", 6, "hypothesis", h))
	run, err := Reduce("run-1", "initial", ledgerTime, entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Entries) != 5 || len(run.State.Hypotheses) != 2 || run.State.Hypotheses[0].ID != "h2" || run.State.Hypotheses[1].ID != "h3" || decoded[Hypothesis](t, run.Entries[2]).Status != "confirmed" {
		t.Fatalf("supersession lost history or current versions: %+v", run)
	}
	for _, target := range []string{"missing", "e1", "h2"} {
		h.Supersedes = target
		invalid := append([]Entry{}, fixtures[1:4]...)
		invalid = append(invalid, entryWith(t, "h2", 5, "hypothesis", h))
		if _, err := Reduce("run-1", "initial", ledgerTime, invalid); err == nil {
			t.Fatalf("accepted invalid supersession %s", target)
		}
	}
}

func TestExpectedBehaviorReplacementRetainsHistory(t *testing.T) {
	entries := ledgerFixtures()
	replacement := ExpectedBehavior{Criteria: []Criterion{{ID: "new", Description: "restore availability", Observation: "readiness"}}}
	entries = append(entries, entryWith(t, "x2", 15, "expected-behavior", replacement))
	run, err := Reduce("run-1", "initial", ledgerTime, entries)
	if err != nil {
		t.Fatal(err)
	}
	if run.State.Expected.ID != "x2" || len(run.Entries) != 12 || run.Entries[4].ID != "x1" || decoded[Validation](t, run.State.Validations[0]).ExpectedEntryID != "x1" || run.ThroughSeq != 15 {
		t.Fatalf("expected replacement mutated history: %+v", run)
	}
}

func TestStrictPayloadDecoding(t *testing.T) {
	for _, e := range ledgerFixtures() {
		if _, err := DecodePayload(e.Kind, e.Payload); err != nil {
			t.Fatalf("valid %s: %v", e.Kind, err)
		}
	}
	for _, tc := range []struct{ name, kind, raw string }{
		{"unknown kind", "other", `{}`},
		{"not object", "evidence", `[]`},
		{"null object", "evidence", `null`},
		{"trailing value", "evidence", `{"fact":"fact","source":"query"} {}`},
		{"unknown field", "evidence", `{"fact":"fact","source":"query","extra":1}`},
		{"case variant", "evidence", `{"Fact":"fact","source":"query"}`},
		{"duplicate field", "evidence", `{"fact":"old","fact":"new","source":"query"}`},
		{"missing field", "evidence", `{"fact":"fact"}`},
		{"null field", "evidence", `{"fact":null,"source":"query"}`},
		{"empty patch", "metadata", `{}`},
		{"null patch", "metadata", `{"impact":null}`},
		{"phase range", "metadata", `{"phase":7}`},
		{"bad session id", "metadata", `{"session_id":"session\ncontrol"}`},
		{"zero optional time", "metadata", `{"started_at":"0001-01-01T00:00:00Z"}`},
		{"duplicate criteria", "expected-behavior", `{"criteria":[{"id":"same","description":"one","observation":"query","threshold":"","source":""},{"id":"same","description":"two","observation":"query","threshold":"","source":""}]}`},
		{"empty criteria", "expected-behavior", `{"criteria":[]}`},
		{"missing observation", "expected-behavior", `{"criteria":[{"id":"id","description":"one","threshold":"","source":""}]}`},
		{"empty observation", "expected-behavior", `{"criteria":[{"id":"id","description":"one","observation":"","threshold":"","source":""}]}`},
		{"nonpositive window", "expected-behavior", `{"criteria":[{"id":"id","description":"one","observation":"query","threshold":"","source":"","window_seconds":0}]}`},
		{"bad decision", "approval", `{"option_id":"o1","decision":"maybe","statement":"wait","at":"2026-11-12T00:00:00Z","target":"prod"}`},
		{"zero required time", "approval", `{"option_id":"o1","decision":"approved","statement":"yes","at":"0001-01-01T00:00:00Z","target":"prod"}`},
		{"invalid time", "approval", `{"option_id":"o1","decision":"approved","statement":"yes","at":"yesterday","target":"prod"}`},
		{"invalid time offset", "approval", `{"option_id":"o1","decision":"approved","statement":"yes","at":"2026-11-12T00:00:00+24:00","target":"prod"}`},
		{"comma fraction", "approval", `{"option_id":"o1","decision":"approved","statement":"yes","at":"2026-11-12T00:00:00,1Z","target":"prod"}`},
		{"single digit hour", "approval", `{"option_id":"o1","decision":"approved","statement":"yes","at":"2026-11-12T0:00:00Z","target":"prod"}`},
		{"excess precision", "approval", `{"option_id":"o1","decision":"approved","statement":"yes","at":"2026-11-12T00:00:00.1234567891Z","target":"prod"}`},
		{"empty initial violation", "safety-violation", `{"wave":1,"investigators":[],"changes":[]}`},
		{"invalid wave", "safety-violation", `{"wave":3,"investigators":[],"changes":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodePayload(Kind(tc.kind), json.RawMessage(tc.raw)); err == nil {
				t.Fatalf("accepted invalid %s", tc.name)
			}
		})
	}
	for _, tc := range []struct {
		index int
		field string
		value any
	}{
		{3, "confidence", "certain"}, {3, "status", "resolved"}, {3, "supporting", []string{}}, {3, "supporting", []string{"bad.id"}},
		{7, "effect", "delete"}, {7, "status", "done"}, {8, "verdict", "ok"},
		{9, "changes", []map[string]string{{"object": "Pod/name", "field": "", "before_hash": "a", "after_hash": "b", "change": "head"}}},
		{9, "changes", []map[string]string{{"object": "Pod/name", "field": "spec", "before_hash": "a", "after_hash": "b", "change": "unknown"}}},
	} {
		e := ledgerFixtures()[tc.index]
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payload[tc.field] = tc.value
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodePayload(e.Kind, raw); err == nil {
			t.Fatalf("accepted %s %s=%v", e.Kind, tc.field, tc.value)
		}
	}
	for _, raw := range []string{
		`{"wave1":{"investigators":[],"reason":"unknown","mutation_check":"unknown"}}`,
		`{"wave1":{"investigators":[],"reason":"unknown","mutation_check":"not_run","extra":"value"}}`,
		`{"wave1":{"investigators":null,"reason":"unknown","mutation_check":"not_run"}}`,
		`{"wave1":{"investigators":[],"reason":"one","reason":"two","mutation_check":"not_run"}}`,
	} {
		if _, err := DecodePayload("metadata", json.RawMessage(raw)); err == nil {
			t.Fatal("accepted invalid nested payload")
		}
	}
}

func TestReduceRejectsInvalidHistoryAndCopiesInput(t *testing.T) {
	for _, mutate := range []func([]Entry){
		func(e []Entry) { e[1].Seq = e[0].Seq },
		func(e []Entry) { e[0].Seq = 0 },
		func(e []Entry) { e[1].RunID = "another-run" },
		func(e []Entry) { e[1].ID = e[0].ID },
		func(e []Entry) { e[1].ID = strings.Repeat("x", 129) },
		func(e []Entry) { e[1].RecordedAt = time.Time{} },
		func(e []Entry) { e[1].Payload = json.RawMessage(`{"fact":"missing source"}`) },
	} {
		entries := ledgerFixtures()
		mutate(entries)
		if _, err := Reduce("run-1", "problem", ledgerTime, entries); err == nil {
			t.Fatal("accepted invalid history")
		}
	}
	entries := ledgerFixtures()
	run, err := Reduce("run-1", "problem", ledgerTime, entries)
	if err != nil {
		t.Fatal(err)
	}
	entries[1].Payload[0] = '['
	entries[1].ID = "changed"
	if run.Entries[1].ID != "e1" || run.Entries[1].Payload[0] != '{' || run.State.Evidence[0].Payload[0] != '{' {
		t.Fatal("input mutation changed snapshot")
	}
	run.Entries[1].Payload[0] = '['
	if run.State.Evidence[0].Payload[0] != '{' {
		t.Fatal("history mutation changed current state")
	}
	for _, tc := range []struct {
		id string
		at time.Time
	}{{"bad.id", ledgerTime}, {"run-1", time.Time{}}} {
		if _, err := Reduce(tc.id, "problem", tc.at, nil); err == nil {
			t.Fatal("accepted invalid run metadata")
		}
	}
	empty, err := Reduce("run-1", "problem", ledgerTime, nil)
	if err != nil || empty.ThroughSeq != 0 || len(empty.Entries) != 0 || empty.State.Expected != nil || len(empty.State.Actions) != 0 {
		t.Fatalf("empty run fabricated state: %+v %v", empty, err)
	}
}

func TestStrictPayloadNormalizesTimeWithoutInventingFacts(t *testing.T) {
	raw := json.RawMessage(`{"fact":"fact","source":"query","observed_at":"2026-11-12T02:00:00.000000123+01:00"}`)
	v, err := DecodePayload("evidence", raw)
	if err != nil {
		t.Fatal(err)
	}
	e := v.(Evidence)
	if e.ObservedAt.Location() != time.UTC || !e.ObservedAt.Equal(ledgerTime) {
		t.Fatalf("time not normalized: %v", e.ObservedAt)
	}
	if _, err := DecodePayload("action", json.RawMessage(`{"at":"2026-11-12T00:00:00Z","command":"observed external action","result":"unknown","effect":"remediation","status":"unverified"}`)); err != nil {
		t.Fatalf("unapproved action must remain recordable: %v", err)
	}
}

func TestMetadataSupportingObjectsKeepUnknownFieldsUnrecorded(t *testing.T) {
	for _, raw := range []string{
		`{"environment":{"context":"prod"}}`,
		`{"tools":{"available":["kubectl"]}}`,
		`{"wave1":{"reason":"not yet dispatched"}}`,
		`{"wave2":{"mutation_check":"inconclusive"}}`,
		`{"environment":{},"tools":{},"wave1":{}}`,
	} {
		value, err := DecodePayload("metadata", json.RawMessage(raw))
		if err != nil {
			t.Fatalf("partial known facts rejected: %s: %v", raw, err)
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(canonical), "null") {
			t.Fatalf("canonical data invented null: %s", canonical)
		}
		if _, err := DecodePayload("metadata", canonical); err != nil {
			t.Fatalf("round trip rejected: %s: %v", canonical, err)
		}
	}
	for _, raw := range []string{`{"wave1":{"mutation_check":""}}`, `{"wave1":{"mutation_check":"made-up"}}`, `{"environment":{"context":null}}`, `{"tools":{"missing":null}}`} {
		if _, err := DecodePayload("metadata", json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted explicit invalid field: %s", raw)
		}
	}
}

func TestStrictOptionalReferencesMustBeValidWhenSupplied(t *testing.T) {
	for _, tc := range []struct {
		index int
		field string
	}{{3, "supersedes"}, {7, "option_id"}, {7, "approval_id"}, {10, "acknowledges"}} {
		entry := ledgerFixtures()[tc.index]
		var fields map[string]any
		if err := json.Unmarshal(entry.Payload, &fields); err != nil {
			t.Fatal(err)
		}
		fields[tc.field] = ""
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodePayload(entry.Kind, raw); err == nil {
			t.Errorf("accepted explicitly empty %s", tc.field)
		}
	}
}

func TestStrictPayloadByteBoundAndEncoding(t *testing.T) {
	prefix, suffix := `{"fact":"`, `","source":"query"}`
	raw := []byte(prefix + strings.Repeat("x", MaxPayloadBytes-len(prefix)-len(suffix)) + suffix)
	if len(raw) != MaxPayloadBytes {
		t.Fatal("fixture not at exact bound")
	}
	if _, err := DecodePayload("evidence", raw); err != nil {
		t.Fatalf("exact bound rejected: %v", err)
	}
	if _, err := DecodePayload("evidence", append(raw, ' ')); err == nil {
		t.Fatal("oversize payload accepted")
	}
	invalid := []byte(`{"fact":"x","source":"query"}`)
	invalid[9] = 0xff
	if _, err := DecodePayload("evidence", invalid); err == nil {
		t.Fatal("invalid UTF-8 accepted with replacement")
	}
}

func TestPayloadUTCTimesRemainJSONEncodable(t *testing.T) {
	for _, stamp := range []string{"0000-01-01T00:00:00+01:00", "9999-12-31T23:59:59-01:00"} {
		raw, _ := json.Marshal(map[string]string{"fact": "fact", "source": "query", "observed_at": stamp})
		if _, err := DecodePayload(KindEvidence, raw); err == nil {
			t.Errorf("accepted unrepresentable UTC time %s", stamp)
		}
	}
}

func TestOpaqueSessionIdentity(t *testing.T) {
	for _, id := range []string{"../session", "harness:session/one", strings.Repeat("x", 256)} {
		raw, _ := json.Marshal(map[string]string{"session_id": id})
		payload, err := DecodePayload(KindMetadata, raw)
		if err != nil || *payload.(MetadataUpdate).SessionID != id {
			t.Errorf("opaque identity rejected or changed: %v", err)
		}
	}
	for _, id := range []string{"", "with\ncontrol", "with\x7fcontrol", strings.Repeat("x", 257)} {
		raw, _ := json.Marshal(map[string]string{"session_id": id})
		if _, err := DecodePayload(KindMetadata, raw); err == nil {
			t.Errorf("accepted invalid session identity")
		}
	}
}
