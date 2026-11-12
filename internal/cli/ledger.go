package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/store"
)

const ledgerOutputBytes = 8192

type ledgerOptions struct {
	repo, id, sessionID, runID, file, problem string
	kind                                      ledger.Kind
	offset                                    int
}

func isLedgerCommand(command string) bool {
	switch command {
	case "run start", "run resume", "run show", "ledger add":
		return true
	}
	return false
}

func (o *ledgerOptions) set(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	switch name {
	case "--id":
		o.id = value
	case "--session-id":
		o.sessionID = value
	case "--run":
		o.runID = value
	case "--file":
		o.file = value
	case "--offset":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return errors.New("offset must be a nonnegative integer")
		}
		o.offset = n
	}
	return nil
}

func (o *ledgerOptions) validate(command string, positional []string, flags map[string]bool) error {
	allowed := map[string]bool{}
	switch command {
	case "run start":
		allowed["--id"], allowed["--session-id"] = true, true
		if len(positional) != 1 {
			return errors.New("usage: sre run start '<problem>' [--id RUN_ID] [--session-id SESSION_ID]")
		}
		o.problem = positional[0]
	case "ledger add":
		allowed["--run"], allowed["--id"], allowed["--file"] = true, true, true
		if len(positional) != 1 || o.id == "" || o.file == "" {
			return errors.New("usage: sre ledger add KIND --run RUN_ID --id ENTRY_ID --file FILE")
		}
		o.kind = ledger.Kind(positional[0])
		if o.runID == "" {
			o.runID = os.Getenv("SRE_RUN_ID")
		}
		if o.runID == "" {
			return errors.New("ledger writes require --run or SRE_RUN_ID")
		}
	case "run resume", "run show":
		allowed["--offset"] = true
		if len(positional) > 1 || command == "run show" && len(positional) != 1 {
			return errors.New("usage: sre run resume [RUN_ID] or sre run show RUN_ID [--offset N]")
		}
		if len(positional) == 1 {
			o.runID = positional[0]
		} else {
			o.runID = os.Getenv("SRE_RUN_ID")
		}
	}
	for flag := range flags {
		if !allowed[flag] {
			return errors.New("flag does not apply to this ledger command")
		}
	}
	return nil
}

type ledgerResult struct {
	RunID      string       `json:"run_id"`
	ThroughSeq int64        `json:"through_seq"`
	Committed  bool         `json:"committed,omitempty"`
	EntryID    string       `json:"entry_id,omitempty"`
	Seq        int64        `json:"seq,omitempty"`
	View       *ledger.View `json:"view,omitempty"`
}

type ledgerViewLoader func(context.Context, *ledger.Service, string, ledger.RenderOptions) (ledger.View, error)

func loadLedgerView(ctx context.Context, service *ledger.Service, id string, opts ledger.RenderOptions) (ledger.View, error) {
	run, err := service.Load(ctx, id)
	if err != nil {
		return ledger.View{}, err
	}
	return ledger.Render(run, opts)
}

func runLedger(command string, opts ledgerOptions, format string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runLedgerWithView(command, opts, format, stdin, stdout, stderr, loadLedgerView)
}

func runLedgerWithView(command string, opts ledgerOptions, format string, stdin io.Reader, stdout, stderr io.Writer, present ledgerViewLoader) int {
	result, gaps, err := executeLedger(context.Background(), command, opts, stdin, present)
	envelope := Envelope{OK: err == nil, Data: nil, Gaps: gaps, Errors: []string{}}
	if result != nil {
		envelope.Data = result
	}
	code := 0
	if err != nil {
		code = 1
		message := safety.NewRedactor().Redact(err.Error())
		if len(message) > 512 {
			message = "ledger operation failed; inspect inputs and repository access"
		}
		wrapper, wrapErr := safety.NewWrapper()
		if wrapErr != nil {
			message = "ledger operation failed"
		} else {
			message = wrapper.Wrap("ledger error", message)
		}
		envelope.Errors = append(envelope.Errors, message)
	}
	encoded, encodeErr := json.Marshal(envelope)
	if encodeErr != nil || len(encoded)+1 > ledgerOutputBytes {
		if result != nil && result.Committed {
			result.View = nil
			envelope.Gaps = []string{"Write committed; ledger view unavailable. Resume this run; retry an uncertain add with the same entry ID."}
		} else {
			envelope = Envelope{OK: false, Data: nil, Gaps: []string{}, Errors: []string{"ledger output exceeds budget"}}
			code = 1
		}
		encoded, _ = json.Marshal(envelope)
	}
	if format == "text" {
		var out strings.Builder
		if result != nil {
			if result.Committed {
				fmt.Fprintf(&out, "Committed run %s", result.RunID)
				if result.EntryID != "" {
					fmt.Fprintf(&out, " entry %s seq %d", result.EntryID, result.Seq)
				}
				out.WriteByte('\n')
			}
			if result.View != nil {
				out.WriteString(result.View.Text)
			}
		}
		for _, gap := range envelope.Gaps {
			out.WriteString(gap + "\n")
		}
		for _, message := range envelope.Errors {
			out.WriteString(message + "\n")
		}
		encoded = []byte(strings.TrimSuffix(out.String(), "\n"))
	}
	encoded = append(encoded, '\n')
	n, writeErr := stdout.Write(encoded)
	if writeErr != nil || n != len(encoded) {
		if result != nil && result.Committed {
			fmt.Fprintf(stderr, "Write committed for run %s", result.RunID)
			if result.EntryID != "" {
				fmt.Fprintf(stderr, " entry %s seq %d; retry with the same entry ID", result.EntryID, result.Seq)
			}
			fmt.Fprintln(stderr, "; output failed. Resume this run to recover the view.")
		} else {
			fmt.Fprintln(stderr, "failed to write command output")
		}
		return 1
	}
	return code
}

func executeLedger(ctx context.Context, command string, opts ledgerOptions, stdin io.Reader, present ledgerViewLoader) (*ledgerResult, []string, error) {
	gaps := []string{}
	var payload []byte
	if command == "ledger add" {
		reader := stdin
		if opts.file != "-" {
			file, err := os.Open(opts.file)
			if err != nil {
				return nil, gaps, errors.New("cannot open ledger payload file")
			}
			defer file.Close()
			reader = file
		}
		var err error
		payload, err = io.ReadAll(io.LimitReader(reader, ledger.MaxPayloadBytes+1))
		if err != nil {
			return nil, gaps, errors.New("cannot read ledger payload")
		}
		if len(payload) > ledger.MaxPayloadBytes {
			return nil, gaps, errors.New("ledger payload exceeds 64 KiB")
		}
		if _, err := ledger.DecodePayload(opts.kind, payload); err != nil {
			return nil, gaps, err
		}
	}
	root, err := store.ResolveRoot(ctx, opts.repo)
	if err != nil {
		return nil, gaps, err
	}
	db, err := store.Open(ctx, root)
	if err != nil {
		return nil, gaps, err
	}
	defer db.Close()
	service := ledger.New(db, nil)
	result := &ledgerResult{}
	switch command {
	case "run start":
		run, err := service.Start(ctx, ledger.StartRequest{ID: opts.id, Problem: opts.problem, SessionID: opts.sessionID})
		if err != nil {
			return nil, gaps, err
		}
		result.RunID, result.ThroughSeq, result.Committed = run.ID, run.ThroughSeq, true
	case "ledger add":
		entry, err := service.Add(ctx, ledger.AddRequest{RunID: opts.runID, ID: opts.id, Kind: opts.kind, Payload: payload})
		if err != nil {
			return nil, gaps, err
		}
		result.RunID, result.EntryID, result.Seq, result.ThroughSeq, result.Committed = entry.RunID, entry.ID, entry.Seq, entry.Seq, true
	default:
		result.RunID = opts.runID
		if result.RunID == "" {
			run, err := service.Latest(ctx)
			if err != nil {
				return nil, gaps, err
			}
			result.RunID = run.ID
		}
	}
	view, err := present(ctx, service, result.RunID, ledger.RenderOptions{Offset: opts.offset, MaxBytes: 6500, Compact: command != "run show"})
	if err != nil {
		if result.Committed {
			gaps = append(gaps, "Write committed; ledger view unavailable. Resume this run; retry an uncertain add with the same entry ID.")
			return result, gaps, nil
		}
		return nil, gaps, err
	}
	result.View = &view
	result.ThroughSeq = view.ThroughSeq
	return result, gaps, nil
}
