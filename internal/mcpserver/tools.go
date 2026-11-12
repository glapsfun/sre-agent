package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/discover"
	"github.com/glapsfun/sre-agent/internal/incident"
	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/logs"
	"github.com/glapsfun/sre-agent/internal/prom"
	"github.com/glapsfun/sre-agent/internal/report"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/timeline"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type discoverInput struct {
	Context string   `json:"context"`
	Only    []string `json:"only,omitempty"`
	Timeout *string  `json:"timeout,omitempty" jsonschema:"Positive Go duration; default 10s per probe, with a 50s total deadline"`
}
type logsInput struct {
	Context     string  `json:"context"`
	Text        *string `json:"text,omitempty" jsonschema:"Inline log text, exclusive with pod selection"`
	Pod         *string `json:"pod,omitempty"`
	Namespace   *string `json:"namespace,omitempty"`
	Container   *string `json:"container,omitempty"`
	Previous    *bool   `json:"previous,omitempty"`
	Since       *string `json:"since,omitempty" jsonschema:"Positive Go duration"`
	MaxPatterns *int    `json:"max_patterns,omitempty"`
}
type promInput struct {
	Context string  `json:"context"`
	Query   string  `json:"query"`
	URL     *string `json:"url,omitempty"`
	Range   *string `json:"range,omitempty" jsonschema:"Positive Go duration in whole milliseconds"`
	Step    *string `json:"step,omitempty" jsonschema:"Positive Go duration in whole milliseconds"`
	Top     *int    `json:"top,omitempty"`
	Timeout *string `json:"timeout,omitempty" jsonschema:"Positive Go duration"`
}
type timelineInput struct {
	Context         string  `json:"context"`
	Namespace       string  `json:"namespace"`
	Workload        *string `json:"workload,omitempty"`
	Repo            *string `json:"repo,omitempty"`
	AlertmanagerURL *string `json:"alertmanager_url,omitempty"`
	Since           *string `json:"since,omitempty" jsonschema:"Positive Go duration"`
	Timeout         *string `json:"timeout,omitempty" jsonschema:"Positive Go duration"`
}
type recallInput struct {
	Repo        *string `json:"repo,omitempty"`
	Service     string  `json:"service"`
	Symptom     string  `json:"symptom"`
	Fingerprint string  `json:"fingerprint"`
	Top         *int    `json:"top,omitempty"`
}
type resumeInput struct {
	RunID  string  `json:"run_id"`
	Repo   *string `json:"repo,omitempty"`
	Offset *int    `json:"offset,omitempty"`
}
type addInput struct {
	RunID   string          `json:"run_id"`
	ID      string          `json:"id"`
	Kind    ledger.Kind     `json:"kind"`
	Payload json.RawMessage `json:"payload"`
	Repo    *string         `json:"repo,omitempty"`
}
type reportInput struct {
	RunID string  `json:"run_id"`
	Repo  *string `json:"repo,omitempty"`
}

func value[T any](p *T) (v T) {
	if p != nil {
		return *p
	}
	return v
}

func duration(p *string) (time.Duration, error) {
	if p == nil {
		return 0, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil || d <= 0 {
		return 0, errors.New("duration must be positive")
	}
	return d, nil
}

func validContext(s string) error {
	if strings.TrimSpace(s) == "" || strings.ContainsAny(s, "\r\n") {
		return errors.New("nonempty valid context is required")
	}
	return nil
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func schemaFor[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	one, zero := 1, 0.0
	for name, p := range s.Properties {
		// Optional pointers distinguish omitted values from invalid explicit
		// zeros; null is never an alternative to an omitted tool argument.
		if len(p.Types) > 0 {
			for _, typ := range p.Types {
				if typ != "null" {
					p.Type = typ
				}
			}
			p.Types = nil
		}
		if p.Type == "string" && name != "text" {
			p.MinLength = &one
			p.Pattern = `\S`
		}
		if p.Type == "integer" {
			p.ExclusiveMinimum = &zero
			if name == "offset" {
				p.ExclusiveMinimum = nil
				p.Minimum = &zero
			}
		}
		if name == "context" {
			p.MinLength = &one
			p.Pattern = `\S`
		}
		if name == "id" || name == "run_id" {
			p.Pattern = identifier.String()
		}
	}
	if p := s.Properties["only"]; p != nil {
		p.MinItems = &one
		p.Items.Enum = []any{"env", "obs", "aws", "gcloud", "github"}
	}
	if p := s.Properties["symptom"]; p != nil {
		p.Enum = []any{"crashloop", "latency", "errors", "metrics", "oom", "networking", "availability"}
	}
	if s.Properties["payload"] != nil {
		s.Properties["payload"] = &jsonschema.Schema{Type: "object"}
		for _, v := range []struct {
			kind    ledger.Kind
			payload any
		}{
			{ledger.KindMetadata, ledger.MetadataUpdate{}}, {ledger.KindEvidence, ledger.Evidence{}}, {ledger.KindHypothesis, ledger.Hypothesis{}},
			{ledger.KindExpectedBehavior, ledger.ExpectedBehavior{}}, {ledger.KindOption, ledger.Option{}}, {ledger.KindApproval, ledger.Approval{}},
			{ledger.KindAction, ledger.Action{}}, {ledger.KindValidation, ledger.Validation{}}, {ledger.KindSafetyViolation, ledger.SafetyViolation{}},
		} {
			payload, err := jsonschema.ForType(reflect.TypeOf(v.payload), nil)
			if err != nil {
				panic(err)
			}
			if s.Defs == nil {
				s.Defs = map[string]*jsonschema.Schema{}
			}
			for name, def := range payload.Defs {
				s.Defs[name] = def
			}
			payload.Defs = nil
			s.OneOf = append(s.OneOf, &jsonschema.Schema{Properties: map[string]*jsonschema.Schema{"kind": {Enum: []any{string(v.kind)}}, "payload": payload}})
		}
	}
	return s
}

func register[T any](s *Server, name, description, command string, handler func(context.Context, T) envelope) {
	schema := schemaFor[T]()
	resolved, err := schema.Resolve(nil)
	if err != nil {
		panic(err)
	}
	s.commands[name] = command
	s.sdk.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: name != "ledger_add", IdempotentHint: name == "ledger_add"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw := req.Params.Arguments
		var fields map[string]any
		var input T
		invalid := !utf8.Valid(raw) || uniqueKeys(json.NewDecoder(bytes.NewReader(raw))) != nil || json.Unmarshal(raw, &fields) != nil || fields == nil
		for _, v := range fields {
			invalid = invalid || v == nil
		}
		if invalid || resolved.Validate(fields) != nil || json.Unmarshal(raw, &input) != nil {
			return toolResult(failure("tool input error", errors.New("invalid tool arguments; consult the tool input schema"))), nil
		}
		if err := ctx.Err(); err != nil {
			return toolResult(failure("tool error", err)), nil
		}
		return toolResult(handler(ctx, input)), nil
	})
}

func uniqueKeys(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s := key.(string)
			if seen[s] {
				return errors.New("duplicate argument")
			}
			seen[s] = true
		}
		if err := uniqueKeys(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func registerTools(s *Server, d dependencies) {
	register(s, "discover", "Discover read-only environment capabilities using an explicit pinned context.", "sre discover --context CONTEXT --format json", func(ctx context.Context, in discoverInput) envelope {
		timeout, err := duration(in.Timeout)
		if err != nil {
			return failure("discovery error", err)
		}
		opts := discover.Options{Context: in.Context, Only: in.Only, Timeout: timeout}
		if err := validContext(in.Context); err != nil {
			return failure("discovery error", err)
		}
		if err := discover.Validate(opts); err != nil {
			return failure("discovery error", err)
		}
		ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
		defer cancel()
		report, err := d.discover(ctx, opts)
		if err != nil {
			return failure("discovery error", err)
		}
		env := success(report)
		for _, name := range []string{"env", "obs", "aws", "gcloud", "github"} {
			p, ok := report.Probes[name]
			if !ok {
				continue
			}
			for _, gap := range p.Gaps {
				env.Gaps = append(env.Gaps, name+": "+gap)
			}
			if p.Status == discover.Timeout || p.Status == discover.Denied || p.Status == discover.Error {
				env.Errors = append(env.Errors, name+": "+string(p.Status))
			}
		}
		env.OK = len(env.Errors) == 0
		return env
	})
	register(s, "logs_fold", "Fold inline logs or a finite pod log snapshot. Never reads server stdin.", "sre logs fold [--pod POD --namespace NAMESPACE --context CONTEXT] --format json", func(ctx context.Context, in logsInput) envelope {
		if err := validContext(in.Context); err != nil {
			return failure("logs error", err)
		}
		if (in.Text == nil) == (in.Pod == nil) {
			return failure("logs error", errors.New("provide inline text or a pod selection, exclusively"))
		}
		opts := logs.Options{MaxPatterns: value(in.MaxPatterns)}
		var r logs.Report
		var err error
		if in.Text != nil {
			if in.Namespace != nil || in.Container != nil || in.Previous != nil || in.Since != nil {
				return failure("logs error", errors.New("pod options require a pod selection"))
			}
			r, err = logs.Fold(contextReader{ctx, strings.NewReader(*in.Text)}, opts)
		} else {
			since, parseErr := duration(in.Since)
			if parseErr != nil {
				return failure("logs error", parseErr)
			}
			pod := logs.PodOptions{Context: in.Context, Pod: value(in.Pod), Namespace: value(in.Namespace), Container: value(in.Container), Previous: value(in.Previous), Since: since, Options: opts}
			if err := logs.ValidatePod(pod); err != nil {
				return failure("logs error", err)
			}
			r, err = d.foldPod(ctx, pod)
		}
		if err == nil {
			r, err = logs.Present(r)
		}
		if err != nil {
			return failure("logs error", err)
		}
		return success(r)
	})
	register(s, "prom_query", "Summarize a Prometheus range query with an explicit pinned context.", "sre prom query PROMQL --context CONTEXT --format json", func(ctx context.Context, in promInput) envelope {
		if err := validContext(in.Context); err != nil {
			return failure("Prometheus error", err)
		}
		rangeValue, err := duration(in.Range)
		if err != nil {
			return failure("Prometheus error", err)
		}
		step, err := duration(in.Step)
		if err != nil {
			return failure("Prometheus error", err)
		}
		timeout, err := duration(in.Timeout)
		if err != nil {
			return failure("Prometheus error", err)
		}
		opts := prom.Options{Context: in.Context, Query: in.Query, URL: value(in.URL), Range: rangeValue, Step: step, Top: value(in.Top), Timeout: timeout}
		if err := prom.Validate(opts); err != nil {
			return failure("Prometheus error", err)
		}
		r, err := d.query(ctx, opts)
		if err == nil {
			r, err = prom.Present(r)
		}
		if err != nil {
			return failure("Prometheus error", err)
		}
		return success(r)
	})
	register(s, "timeline", "Collect a read-only change timeline with source gaps and an explicit pinned context.", "sre timeline --context CONTEXT --namespace NAMESPACE --format json", func(ctx context.Context, in timelineInput) envelope {
		if err := validContext(in.Context); err != nil {
			return failure("timeline error", err)
		}
		since, err := duration(in.Since)
		if err != nil {
			return failure("timeline error", err)
		}
		timeout, err := duration(in.Timeout)
		if err != nil {
			return failure("timeline error", err)
		}
		opts := timeline.Options{Context: in.Context, Namespace: in.Namespace, Workload: value(in.Workload), Repo: value(in.Repo), AlertmanagerURL: value(in.AlertmanagerURL), Since: since, Timeout: timeout}
		if err := timeline.Validate(opts); err != nil {
			return failure("timeline error", err)
		}
		r, err := d.collect(ctx, opts)
		if err != nil {
			return failure("timeline error", err)
		}
		return presentTimeline(ctx, r)
	})
	register(s, "recall", "Retrieve historical incident candidates; confirm against current evidence before applying fixes.", "sre recall --service SERVICE --symptom SYMPTOM --fingerprint FINGERPRINT --repo PATH --format json", func(ctx context.Context, in recallInput) envelope {
		db, err := d.open(ctx, value(in.Repo))
		if err != nil {
			return failure("incident error", err)
		}
		defer db.Close()
		r, err := incident.New(db, d.now).Recall(ctx, incident.RecallOptions{Service: in.Service, Symptom: in.Symptom, Fingerprint: in.Fingerprint, Top: value(in.Top)})
		if err != nil {
			return failure("incident error", err)
		}
		begin, end, err := boundary("incident candidates")
		if err != nil {
			return failure("incident error", err)
		}
		for i := range r.Matches {
			r.Matches[i].Incident.RunID = safety.NewRedactor().Redact(r.Matches[i].Incident.RunID)
		}
		env := success(struct {
			Begin string `json:"begin"`
			incident.RecallResult
			Finish string `json:"finish"`
		}{begin, r, end})
		env.Gaps = append(env.Gaps, "Historical candidates only; confirm against current evidence and obtain approval, dry-run and validation before applying any fix.")
		return env
	})
	register(s, "run_resume", "Read an explicit run and its compact ledger view without binding a session.", "sre run show RUN_ID --repo PATH --format json", func(ctx context.Context, in resumeInput) envelope {
		db, err := d.open(ctx, value(in.Repo))
		if err != nil {
			return failure("ledger error", err)
		}
		defer db.Close()
		run, err := ledger.New(db, d.now).Load(ctx, in.RunID)
		if err != nil {
			return failure("ledger error", err)
		}
		view, err := ledger.Render(run, ledger.RenderOptions{Offset: value(in.Offset), MaxBytes: 6500, Compact: true})
		if err != nil {
			return failure("ledger error", err)
		}
		return success(&ledgerOutput{RunID: run.ID, ThroughSeq: view.ThroughSeq, View: &view})
	})
	register(s, "ledger_add", "Append an entry with explicit run and entry IDs. Requires separate orchestrator write permission; never grant to investigators. Retrying the same entry ID and payload is idempotent.", "sre run show RUN_ID --repo PATH --format json", func(ctx context.Context, in addInput) envelope {
		if _, err := ledger.DecodePayload(in.Kind, in.Payload); err != nil {
			return failure("ledger error", err)
		}
		db, err := d.open(ctx, value(in.Repo))
		if err != nil {
			return failure("ledger error", err)
		}
		defer db.Close()
		service := ledger.New(db, d.now)
		entry, err := service.Add(ctx, ledger.AddRequest{RunID: in.RunID, ID: in.ID, Kind: in.Kind, Payload: in.Payload})
		if err != nil {
			return failure("ledger error", err)
		}
		receipt := &ledgerOutput{RunID: entry.RunID, EntryID: entry.ID, Seq: entry.Seq, ThroughSeq: entry.Seq, Committed: true}
		env := success(receipt)
		run, err := service.Load(ctx, entry.RunID)
		var view ledger.View
		if err == nil {
			view, err = ledger.Render(run, ledger.RenderOptions{MaxBytes: 6500, Compact: true})
		}
		if err != nil {
			env.Gaps = append(env.Gaps, "Write committed; ledger view unavailable. Resume this run; retry an uncertain add with the same entry ID.")
		} else {
			receipt.View = &view
			receipt.ThroughSeq = view.ThroughSeq
		}
		return env
	})
	register(s, "report", "Generate the existing investigation report document for an explicit run; no file output.", "sre report RUN_ID --repo PATH --format json", func(ctx context.Context, in reportInput) envelope {
		begin, end, err := boundary("investigation report")
		if err != nil {
			return failure("report error", err)
		}
		db, err := d.open(ctx, value(in.Repo))
		if err != nil {
			return failure("report error", err)
		}
		defer db.Close()
		doc, err := report.Generate(ctx, ledger.New(db, d.now), in.RunID)
		if err != nil {
			return failure("report error", err)
		}
		return success(struct {
			Begin string `json:"begin"`
			report.Document
			Finish string `json:"finish"`
		}{begin, doc, end})
	})
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func presentTimeline(ctx context.Context, r timeline.Report) envelope {
	clean, err := timeline.RedactReport(ctx, r)
	if err != nil {
		return failure("timeline error", err)
	}
	wrapper, err := safety.NewWrapper()
	if err != nil {
		return failure("timeline error", err)
	}
	b := strings.Split(wrapper.Wrap("timeline", ""), "\n")
	env := success(struct {
		Begin string `json:"external_data_begin"`
		timeline.Report
		End string `json:"external_data_end"`
	}{b[0], clean, b[2]})
	for _, gap := range clean.Gaps {
		env.Gaps = append(env.Gaps, wrapper.Wrap("timeline gap", gap.Source+": "+gap.Code+": "+gap.Message))
	}
	for _, message := range clean.Errors {
		env.Errors = append(env.Errors, wrapper.Wrap("timeline error", message))
	}
	env.OK = len(env.Errors) == 0
	return env
}
