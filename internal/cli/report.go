package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/report"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/store"
)

type reportOptions struct {
	repo, runID, out, rules string
	snapshot                report.SnapshotOptions
}

type reportOutput struct {
	Begin string `json:"begin"`
	report.Snapshot
	Path       string `json:"path,omitempty"`
	DetailPath string `json:"detail_path,omitempty"`
	IndexPath  string `json:"index_path,omitempty"`
	Written    bool   `json:"written,omitempty"`
	Finish     string `json:"finish"`
}

func runReport(opts reportOptions, format string, stdout, stderr io.Writer, now func() time.Time) int {
	envelope := Envelope{Gaps: []string{}, Errors: []string{}}
	// Prepare the boundary before writing so presentation setup cannot obscure
	// a successful file export.
	wrapper, err := safety.NewWrapper()
	var output reportOutput
	if err != nil {
		envelope.Errors = append(envelope.Errors, "report presentation unavailable")
	} else {
		opts.snapshot.Now = now
		var document report.Snapshot
		var receipt report.ExportReceipt
		document, err = executeReport(context.Background(), opts)
		if err == nil && opts.out != "" {
			receipt, err = report.Export(opts.out, document)
		}
		if err != nil {
			envelope.Errors = append(envelope.Errors, wrapper.Wrap("report error", safety.NewRedactor().Redact(err.Error())))
		} else {
			boundary := strings.Split(wrapper.Wrap("investigation report", ""), "\n")
			output = reportOutput{Begin: boundary[0], Snapshot: document, Finish: boundary[2]}
			if opts.out != "" {
				output.Markdown = ""
				output.Detail = ""
				output.Path = safety.NewRedactor().Redact(receipt.Path)
				output.DetailPath = safety.NewRedactor().Redact(receipt.DetailPath)
				output.IndexPath = safety.NewRedactor().Redact(receipt.IndexPath)
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
				if output.Detail != "" {
					fmt.Fprint(&text, "\n", output.Detail)
				}
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

func executeReport(ctx context.Context, opts reportOptions) (report.Snapshot, error) {
	if opts.rules != "" {
		file, err := os.Open(opts.rules)
		if err != nil {
			return report.Snapshot{}, err
		}
		defer file.Close()
		opts.snapshot.Rules, err = report.ReadPolicy(file)
		if err != nil {
			return report.Snapshot{}, err
		}
	}
	root, err := store.ResolveRoot(ctx, opts.repo)
	if err != nil {
		return report.Snapshot{}, err
	}
	db, err := store.OpenReadOnly(ctx, root)
	if err != nil {
		return report.Snapshot{}, err
	}
	defer db.Close()
	return report.GenerateSnapshot(ctx, ledger.New(db, nil), opts.runID, opts.snapshot)
}
