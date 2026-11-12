package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/glapsfun/sre-agent/internal/incident"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/store"
)

type incidentOptions struct {
	repo, runID, service, symptom, fingerprint string
	top                                        int
}

type captureResult struct {
	RunID      string             `json:"run_id"`
	ThroughSeq int64              `json:"through_seq"`
	Committed  bool               `json:"committed"`
	Begin      string             `json:"begin,omitempty"`
	Incident   *incident.Incident `json:"incident,omitempty"`
	Finish     string             `json:"finish,omitempty"`
}

type recallReport struct {
	Begin string `json:"begin"`
	incident.RecallResult
	Finish string `json:"finish"`
}

const incidentCandidates = "Historical candidates only; confirm against current evidence and obtain approval, dry-run and validation before applying any fix."
const capturePresentationGap = "Write committed; incident view unavailable. Retry capture with the same explicit run and signature to recover the view."

func runIncident(command string, opts incidentOptions, format string, stdout, stderr io.Writer) int {
	return runIncidentWithWrapper(command, opts, format, stdout, stderr, safety.NewWrapper)
}

func runIncidentWithWrapper(command string, opts incidentOptions, format string, stdout, stderr io.Writer, newWrapper func() (*safety.Wrapper, error)) int {
	ctx := context.Background()
	envelope := Envelope{Gaps: []string{}, Errors: []string{}}
	var receipt *captureResult
	data, err := executeIncident(ctx, command, opts)
	if err == nil {
		wrapper, wrapErr := newWrapper()
		if captured, ok := data.(incident.Incident); ok {
			// Capture has committed before presentation. Keep its receipt even if the
			// boundary cannot be created or the output writer fails.
			receipt = &captureResult{RunID: captured.RunID, ThroughSeq: captured.ThroughSeq, Committed: true}
			envelope.Data = receipt
			if wrapErr != nil {
				envelope.Gaps = append(envelope.Gaps, capturePresentationGap)
			} else {
				boundary := strings.Split(wrapper.Wrap("captured incident", ""), "\n")
				receipt.Begin, receipt.Incident, receipt.Finish = boundary[0], &captured, boundary[2]
			}
		} else if wrapErr != nil {
			err = fmt.Errorf("incident presentation unavailable")
		} else {
			result := data.(incident.RecallResult)
			// Stored row identities are historical external data too. The service
			// already redacts the signature and typed record on its read boundary.
			redactor := safety.NewRedactor()
			for i := range result.Matches {
				result.Matches[i].Incident.RunID = redactor.Redact(result.Matches[i].Incident.RunID)
			}
			boundary := strings.Split(wrapper.Wrap("incident candidates", ""), "\n")
			envelope.Data = recallReport{Begin: boundary[0], RecallResult: result, Finish: boundary[2]}
			envelope.Gaps = append(envelope.Gaps, incidentCandidates)
		}
	}
	envelope.OK = err == nil
	code := 0
	if err != nil {
		code = 1
		message := safety.NewRedactor().Redact(err.Error())
		wrapper, wrapErr := newWrapper()
		if wrapErr != nil {
			message = "incident operation failed"
		} else {
			message = wrapper.Wrap("incident error", message)
		}
		envelope.Errors = append(envelope.Errors, message)
	}
	var encoded []byte
	if format == "text" {
		encoded, err = incidentText(envelope)
	} else {
		encoded, err = json.Marshal(envelope)
	}
	if err != nil {
		if receipt == nil {
			fmt.Fprintln(stderr, "failed to encode command output")
			return 1
		}
		receipt.Begin, receipt.Incident, receipt.Finish = "", nil, ""
		envelope.Gaps = []string{capturePresentationGap}
		if format == "text" {
			encoded, _ = incidentText(envelope)
		} else {
			encoded, _ = json.Marshal(envelope)
		}
	}
	encoded = append(encoded, '\n')
	n, writeErr := stdout.Write(encoded)
	if writeErr != nil || n != len(encoded) {
		if receipt != nil {
			fmt.Fprintf(stderr, "Write committed for run %s through sequence %d; output failed. Retry capture with the same explicit run and signature.\n", receipt.RunID, receipt.ThroughSeq)
		} else {
			fmt.Fprintln(stderr, "failed to write command output")
		}
		return 1
	}
	return code
}

func executeIncident(ctx context.Context, command string, opts incidentOptions) (any, error) {
	root, err := store.ResolveRoot(ctx, opts.repo)
	if err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, root)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	service := incident.New(db, nil)
	if command == "incident add" {
		return service.Capture(ctx, incident.CaptureOptions{RunID: opts.runID, Service: opts.service, Symptom: opts.symptom, Fingerprint: opts.fingerprint})
	}
	return service.Recall(ctx, incident.RecallOptions{Service: opts.service, Symptom: opts.symptom, Fingerprint: opts.fingerprint, Top: opts.top})
}

func incidentText(envelope Envelope) ([]byte, error) {
	var out strings.Builder
	switch data := envelope.Data.(type) {
	case *captureResult:
		fmt.Fprintf(&out, "Committed incident for run %s through sequence %d\n", data.RunID, data.ThroughSeq)
		if data.Incident != nil {
			record, err := json.MarshalIndent(data.Incident, "", "  ")
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&out, "%s\n%s\n%s\n", data.Begin, record, data.Finish)
		}
	case recallReport:
		fmt.Fprintf(&out, "Incident candidates: %d of %d\n%s\n", len(data.Matches), data.Total, data.Begin)
		if data.Message != "" {
			fmt.Fprintln(&out, data.Message)
		}
		for _, match := range data.Matches {
			fmt.Fprintf(&out, "Candidate score: %d, similarity: %g\n", match.Score, match.Similarity)
			for _, reason := range match.Reasons {
				fmt.Fprintln(&out, reason)
			}
			record, err := json.MarshalIndent(match.Incident, "", "  ")
			if err != nil {
				return nil, err
			}
			fmt.Fprintln(&out, string(record))
		}
		fmt.Fprintln(&out, data.Finish)
	}
	for _, gap := range envelope.Gaps {
		fmt.Fprintln(&out, gap)
	}
	for _, message := range envelope.Errors {
		fmt.Fprintln(&out, message)
	}
	return []byte(strings.TrimSuffix(out.String(), "\n")), nil
}
