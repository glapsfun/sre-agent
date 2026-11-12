package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/report"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/store"
)

type reportOptions struct{ repo, runID, out string }

type reportOutput struct {
	Begin      string   `json:"begin"`
	RunID      string   `json:"run_id"`
	ThroughSeq int64    `json:"through_seq"`
	Status     string   `json:"status"`
	Markdown   string   `json:"markdown,omitempty"`
	Gaps       []string `json:"gaps"`
	Path       string   `json:"path,omitempty"`
	Written    bool     `json:"written,omitempty"`
	Finish     string   `json:"finish"`
}

func runReport(opts reportOptions, format string, stdout, stderr io.Writer) int {
	envelope := Envelope{Gaps: []string{}, Errors: []string{}}
	// Prepare the boundary before writing so presentation setup cannot obscure
	// a successful file replacement.
	wrapper, err := safety.NewWrapper()
	var output reportOutput
	if err != nil {
		envelope.Errors = append(envelope.Errors, "report presentation unavailable")
	} else {
		var document report.Document
		document, err = executeReport(context.Background(), opts)
		if err == nil && opts.out != "" {
			err = report.WriteFile(opts.out, document)
		}
		if err != nil {
			envelope.Errors = append(envelope.Errors, wrapper.Wrap("report error", safety.NewRedactor().Redact(err.Error())))
		} else {
			boundary := strings.Split(wrapper.Wrap("investigation report", ""), "\n")
			output = reportOutput{Begin: boundary[0], RunID: document.RunID, ThroughSeq: document.ThroughSeq, Status: document.Status, Markdown: document.Markdown, Gaps: document.Gaps, Finish: boundary[2]}
			if opts.out != "" {
				output.Markdown = ""
				output.Path = safety.NewRedactor().Redact(opts.out)
				output.Written = true
			}
			envelope.Data = output
		}
	}
	envelope.OK = err == nil
	var encoded []byte
	if format == "text" {
		var text strings.Builder
		if envelope.OK {
			fmt.Fprintln(&text, output.Begin)
			if output.Written {
				fmt.Fprintf(&text, "Report file written to %q for run %s through sequence %d; status: %s\n", output.Path, output.RunID, output.ThroughSeq, output.Status)
			} else {
				fmt.Fprint(&text, output.Markdown)
			}
			for _, gap := range output.Gaps {
				fmt.Fprintln(&text, "Gap:", gap)
			}
			fmt.Fprintln(&text, output.Finish)
		} else {
			fmt.Fprintln(&text, strings.Join(envelope.Errors, "\n"))
		}
		encoded = []byte(text.String())
	} else {
		encoded, err = json.Marshal(envelope)
		if err != nil {
			fmt.Fprintln(stderr, "failed to encode command output")
			return 1
		}
		encoded = append(encoded, '\n')
	}
	n, writeErr := stdout.Write(encoded)
	if writeErr != nil || n != len(encoded) {
		if output.Written {
			fmt.Fprintln(stderr, "Report file written; failed to write command output.")
		} else {
			fmt.Fprintln(stderr, "failed to write command output")
		}
		return 1
	}
	if !envelope.OK {
		return 1
	}
	return 0
}

func executeReport(ctx context.Context, opts reportOptions) (report.Document, error) {
	root, err := store.ResolveRoot(ctx, opts.repo)
	if err != nil {
		return report.Document{}, err
	}
	db, err := store.Open(ctx, root)
	if err != nil {
		return report.Document{}, err
	}
	defer db.Close()
	return report.Generate(ctx, ledger.New(db, nil), opts.runID)
}
