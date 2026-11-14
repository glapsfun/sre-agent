package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/glapsfun/sre-agent/internal/collect"
	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/sources"
	"github.com/glapsfun/sre-agent/internal/store"
)

type collectOptions struct {
	repo, format, plan, program, run, id string
	mode                                 collect.Mode
	approveCode                          bool
	timeout                              time.Duration
}

func parseCollect(args []string) (collectOptions, error) {
	opts := collectOptions{format: "json", mode: collect.ModeAuto, timeout: 10 * time.Second}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		name, value, has := strings.Cut(args[i], "=")
		if seen[name] {
			return opts, errors.New("duplicate collection flag")
		}
		seen[name] = true
		switch name {
		case "--approve-code":
			if !has {
				value, has = "true", true
			}
		case "--repo", "--format", "--plan", "--program", "--run", "--id", "--mode", "--timeout":
		default:
			return opts, errors.New("unknown collection flag or positional argument")
		}
		if !has {
			i++
			if i == len(args) {
				return opts, errors.New("collection flag requires a value")
			}
			value = args[i]
		}
		if strings.TrimSpace(value) == "" {
			return opts, errors.New("collection flag value must not be empty")
		}
		switch name {
		case "--repo":
			opts.repo = value
		case "--format":
			opts.format = value
		case "--plan":
			opts.plan = value
		case "--program":
			opts.program = value
		case "--run":
			opts.run = value
		case "--id":
			opts.id = value
		case "--mode":
			opts.mode = collect.Mode(value)
		case "--timeout":
			v, err := time.ParseDuration(value)
			if err != nil || v <= 0 || v > 60*time.Second {
				return opts, errors.New("collection timeout must be positive and at most 60s")
			}
			opts.timeout = v
		case "--approve-code":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return opts, errors.New("collection approval must be true or false")
			}
			opts.approveCode = v
		}
	}
	if opts.plan == "" || !sourceLedgerID.MatchString(opts.run) || !sourceLedgerID.MatchString(opts.id) {
		return opts, errors.New("collection requires --plan and valid --run and --id")
	}
	if opts.mode != collect.ModeAuto && opts.mode != collect.ModeOrdinary && opts.mode != collect.ModeCode {
		return opts, errors.New("collection mode must be auto, ordinary or code")
	}
	if opts.format != "json" && opts.format != "text" {
		return opts, errors.New("collection format must be json or text")
	}
	if opts.plan == "-" && opts.program == "-" {
		return opts, errors.New("collection plan and program cannot both read stdin")
	}
	return opts, nil
}

type collectionOutput struct {
	Begin     string             `json:"begin,omitempty"`
	Result    *collect.Result    `json:"result,omitempty"`
	Finish    string             `json:"finish,omitempty"`
	Receipt   *collectionReceipt `json:"receipt,omitempty"`
	Committed bool               `json:"committed,omitempty"`
	Replayed  bool               `json:"replayed,omitempty"`
	RunID     string             `json:"run_id,omitempty"`
	EntryID   string             `json:"entry_id,omitempty"`
	Seq       int64              `json:"seq,omitempty"`
}

func runCollect(args []string, stdin io.Reader, stdout, stderr io.Writer, now func() time.Time) int {
	return runCollectWithService(args, stdin, stdout, stderr, collect.Service{Sources: sources.Service{Now: now}})
}

func runCollectWithService(args []string, stdin io.Reader, stdout, stderr io.Writer, service collect.Service) int {
	opts, err := parseCollect(args)
	out, gaps := &collectionOutput{}, []string{}
	if err == nil {
		out, gaps, err = executeCollect(context.Background(), opts, stdin, service)
	}
	return presentCollection(opts.format, out, gaps, err, stdout, stderr)
}

func presentCollection(format string, out *collectionOutput, gaps []string, err error, stdout, stderr io.Writer) int {
	envelope := Envelope{OK: err == nil, Data: out, Gaps: gaps, Errors: []string{}}
	if err != nil {
		envelope.Errors = append(envelope.Errors, safety.NewRedactor().Redact(err.Error()))
	}
	if out.Result != nil {
		wrapper, wrapErr := safety.NewWrapper()
		if wrapErr != nil {
			out.Result = nil
			envelope.OK = false
			envelope.Errors = append(envelope.Errors, "Collection result boundary unavailable.")
			envelope.Gaps = append(envelope.Gaps, "Collection result omitted because external-data boundaries were unavailable.")
		} else {
			boundary := strings.Split(wrapper.Wrap("collection", ""), "\n")
			out.Begin, out.Finish = boundary[0], boundary[2]
		}
	}
	marshal := func() ([]byte, error) {
		if format == "text" {
			return json.MarshalIndent(envelope, "", "  ")
		}
		return json.Marshal(envelope)
	}
	raw, encodeErr := marshal()
	if encodeErr != nil || len(raw)+1 > 2<<20 {
		out.Result = nil
		out.Begin, out.Finish = "", ""
		envelope.OK = false
		envelope.Gaps = append(envelope.Gaps, "Collection result exceeded output budget or could not be encoded; raw data omitted.")
		envelope.Errors = append(envelope.Errors, "Collection result presentation incomplete.")
		raw, encodeErr = marshal()
	}
	if encodeErr != nil || len(raw)+1 > 2<<20 {
		collectionWriteRecovery(stderr, out)
		return 1
	}
	raw = append(raw, '\n')
	n, writeErr := stdout.Write(raw)
	if writeErr != nil || n != len(raw) {
		collectionWriteRecovery(stderr, out)
		return 1
	}
	if !envelope.OK {
		return 1
	}
	return 0
}

func collectionWriteRecovery(stderr io.Writer, out *collectionOutput) {
	if out.Committed {
		fmt.Fprintf(stderr, "Collection receipt committed for run %s entry %s seq %d; output failed. Resume the run; do not repeat collection automatically.\n", out.RunID, out.EntryID, out.Seq)
	} else {
		fmt.Fprintln(stderr, "Collection output failed; do not repeat collection automatically.")
	}
}

func executeCollect(ctx context.Context, opts collectOptions, stdin io.Reader, service collect.Service) (*collectionOutput, []string, error) {
	out, gaps := &collectionOutput{}, []string{}
	raw, err := readSourceInput(opts.plan, stdin, 64<<10)
	if err != nil {
		return out, gaps, errors.New("collection plan input unavailable or exceeds limit")
	}
	plan, err := collect.DecodePlan(raw)
	if err != nil {
		return out, gaps, collect.ErrInvalidPlan
	}
	req := collect.Request{Plan: plan, Mode: opts.mode, CodeApproved: opts.approveCode, Timeout: opts.timeout}
	if req.Mode == "" {
		req.Mode = collect.ModeAuto
	}
	if req.Timeout == 0 {
		req.Timeout = 10 * time.Second
	}
	if opts.program != "" {
		program, err := readSourceInput(opts.program, stdin, 64<<10)
		if err != nil || !utf8.Valid(program) {
			return out, gaps, errors.New("collection program must be UTF-8 and at most 64KiB")
		}
		req.Program = string(program)
	}
	root, err := store.ResolveRoot(ctx, opts.repo)
	if err != nil {
		return out, gaps, errors.New("collection repository unavailable")
	}
	out.RunID, out.EntryID = opts.run, opts.id
	digest := collectionIdentity(req)
	check := func(checkCtx context.Context) (bool, error) {
		db, err := store.OpenReadOnly(checkCtx, root)
		if err != nil {
			return false, errors.New("existing collection run unavailable")
		}
		run, err := ledger.New(db, nil).Load(checkCtx, opts.run)
		_ = db.Close()
		if err != nil {
			return false, errors.New("existing collection run unavailable")
		}
		for _, entry := range run.Entries {
			if entry.ID != opts.id {
				continue
			}
			if entry.Kind != ledger.KindEvidence {
				return false, ledger.ErrConflict
			}
			value, err := ledger.DecodePayload(entry.Kind, entry.Payload)
			if err != nil {
				return false, ledger.ErrConflict
			}
			evidence := value.(ledger.Evidence)
			if evidence.Source != "collection" {
				return false, ledger.ErrConflict
			}
			receipt, err := decodeCollectionReceipt(evidence.Fact, req, digest)
			if err != nil {
				return false, ledger.ErrConflict
			}
			out.Receipt, out.Committed, out.Replayed, out.Seq = receipt, true, true, entry.Seq
			gaps = append(gaps, "Replayed stored collection receipt; source calls and runtime were not repeated. Raw results were not stored.")
			if evidence.Gap != "" {
				gaps = append(gaps, evidence.Gap)
			}
			if !receipt.Success {
				return true, collect.ErrIncompleteCollection
			}
			return true, nil
		}
		return false, nil
	}
	if found, err := check(ctx); found || err != nil {
		return out, gaps, err
	}
	release, err := lockCollection(root)
	if err != nil {
		return out, gaps, err
	}
	defer release()
	if found, err := check(ctx); found || err != nil {
		return out, gaps, err
	}
	result, collectErr := service.Collect(ctx, root, req)
	out.Result = &result
	now := service.Sources.Now
	if now == nil {
		now = time.Now
	}
	receipt, evidence, payload, writeErr := projectCollection(req, result, collectErr == nil, digest, now())
	out.Receipt = receipt
	if evidence.Gap != "" {
		gaps = append(gaps, evidence.Gap)
	}
	// Collection cancellation must not cancel the bounded receipt write.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if writeErr == nil {
		var writer *store.Store
		writer, writeErr = store.Open(cleanup, root)
		if writeErr == nil {
			var entry ledger.Entry
			entry, writeErr = ledger.New(writer, now).Add(cleanup, ledger.AddRequest{RunID: opts.run, ID: opts.id, Kind: ledger.KindEvidence, Payload: payload})
			_ = writer.Close()
			if writeErr == nil {
				out.Committed, out.Seq = true, entry.Seq
			}
		}
	}
	if writeErr != nil {
		gaps = append(gaps, "Collection returned but its receipt was not recorded. Do not repeat collection automatically.")
		return out, gaps, errors.New("collection receipt ledger write failed")
	}
	if collectErr != nil {
		return out, gaps, collect.ErrIncompleteCollection
	}
	return out, gaps, nil
}

func lockCollection(root string) (func(), error) {
	directory := filepath.Join(root, ".sre-agent")
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return nil, errors.New("collection store must be an existing directory")
	}
	confined, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("collection store unavailable")
	}
	opened, err := confined.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = confined.Close()
		return nil, errors.New("collection store changed while opening")
	}
	if err := confined.Mkdir("collect.lock", 0700); err != nil {
		_ = confined.Close()
		return nil, errors.New("collection busy; inspect the active or stale .sre-agent/collect.lock before retrying")
	}
	owned, err := confined.Lstat("collect.lock")
	if err != nil {
		_ = confined.Close()
		return nil, errors.New("collection lock unavailable")
	}
	return func() {
		current, err := confined.Lstat("collect.lock")
		if err == nil && os.SameFile(owned, current) {
			_ = confined.Remove("collect.lock")
		}
		_ = confined.Close()
	}, nil
}
