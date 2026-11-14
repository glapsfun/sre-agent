package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/sources"
	"github.com/glapsfun/sre-agent/internal/store"
)

type sourceOptions struct {
	command, repo, format, file, known, args, run, id, source, tool, context string
	approve, scout                                                           bool
	timeout                                                                  time.Duration
}

func parseSources(args []string) (sourceOptions, error) {
	opts := sourceOptions{format: "json"}
	var positional []string
	flags := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name, value, has := strings.Cut(arg, "=")
		if flags[name] {
			return opts, errors.New("duplicate source flag")
		}
		flags[name] = true
		switch name {
		case "--approve", "--approve-scout":
			if !has {
				value = "true"
				has = true
			}
		case "--repo", "--format", "--file", "--known", "--args", "--run", "--id", "--context", "--timeout":
		default:
			return opts, errors.New("unknown source flag")
		}
		if !has {
			i++
			if i == len(args) {
				return opts, errors.New("source flag requires a value")
			}
			value = args[i]
		}
		if strings.TrimSpace(value) == "" {
			return opts, errors.New("source flag value must not be empty")
		}
		switch name {
		case "--repo":
			opts.repo = value
		case "--format":
			opts.format = value
		case "--file":
			opts.file = value
		case "--known":
			opts.known = value
		case "--args":
			opts.args = value
		case "--run":
			opts.run = value
		case "--id":
			opts.id = value
		case "--context":
			opts.context = value
		case "--timeout":
			v, err := time.ParseDuration(value)
			if err != nil || v <= 0 || v > 60*time.Second {
				return opts, errors.New("source timeout must be positive and at most 60s")
			}
			opts.timeout = v
		case "--approve", "--approve-scout":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return opts, errors.New("source approval must be true or false")
			}
			if name == "--approve" {
				opts.approve = v
			} else {
				opts.scout = v
			}
		}
	}
	if len(positional) == 0 || opts.format != "json" && opts.format != "text" {
		return opts, errors.New("usage: sre sources list|scout|approve|call [--repo PATH] [--format json|text]")
	}
	opts.command = positional[0]
	allowed := map[string]bool{"--repo": true, "--format": true}
	want := 1
	switch opts.command {
	case "list":
	case "scout":
		for _, f := range []string{"--approve-scout", "--known", "--context", "--timeout"} {
			allowed[f] = true
		}
	case "approve":
		allowed["--approve"], allowed["--file"] = true, true
		if opts.file == "" {
			return opts, errors.New("source approval requires --file")
		}
	case "call":
		want = 3
		for _, f := range []string{"--args", "--run", "--id", "--timeout"} {
			allowed[f] = true
		}
		if opts.args == "" || opts.run == "" || opts.id == "" {
			return opts, errors.New("source call requires --args, --run and --id")
		}
	default:
		return opts, errors.New("unknown sources command")
	}
	if len(positional) != want {
		return opts, errors.New("invalid sources command arguments")
	}
	for flag := range flags {
		if !allowed[flag] {
			return opts, errors.New("flag does not apply to sources command")
		}
	}
	if want == 3 {
		opts.source, opts.tool = positional[1], positional[2]
	}
	return opts, nil
}

type sourceOutput struct {
	Begin     string `json:"begin,omitempty"`
	Result    any    `json:"result,omitempty"`
	Finish    string `json:"finish,omitempty"`
	Committed bool   `json:"committed,omitempty"`
	Saved     bool   `json:"saved,omitempty"`
	Replayed  bool   `json:"replayed,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	EntryID   string `json:"entry_id,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
}

func runSources(args []string, stdin io.Reader, stdout, stderr io.Writer, now func() time.Time) int {
	opts, err := parseSources(args)
	result := &sourceOutput{}
	gaps := []string{}
	if err == nil {
		result, gaps, err = executeSources(context.Background(), opts, stdin, sources.Service{Now: now})
	}
	envelope := Envelope{OK: err == nil, Data: result, Gaps: gaps, Errors: []string{}}
	code := 0
	if err != nil {
		code = 1
		envelope.Errors = append(envelope.Errors, safety.NewRedactor().Redact(err.Error()))
	}
	if result.Result != nil {
		wrapper, wrapErr := safety.NewWrapper()
		if wrapErr != nil {
			result.Result = nil
			envelope.Gaps = append(envelope.Gaps, "Source result presentation unavailable; inspect the durable receipt if present.")
		} else {
			boundary := strings.Split(wrapper.Wrap("investigation sources", ""), "\n")
			result.Begin, result.Finish = boundary[0], boundary[2]
		}
	}
	encoded, encodeErr := json.Marshal(envelope)
	if encodeErr != nil || len(encoded) > 2<<20 {
		result.Result = nil
		envelope.Gaps = append(envelope.Gaps, "Source result exceeded output budget; raw data omitted.")
		encoded, _ = json.Marshal(envelope)
	}
	if opts.format == "text" {
		encoded, _ = json.MarshalIndent(envelope, "", "  ")
	}
	encoded = append(encoded, '\n')
	n, writeErr := stdout.Write(encoded)
	if writeErr != nil || n != len(encoded) {
		if result.Saved {
			fmt.Fprintln(stderr, "Source approval saved; output failed. Use sources list to inspect it.")
		} else if result.Committed {
			fmt.Fprintf(stderr, "Source evidence committed for run %s entry %s seq %d; output failed. Resume the run; do not repeat collection automatically.\n", result.RunID, result.EntryID, result.Seq)
		} else {
			fmt.Fprintln(stderr, "Source output failed; do not repeat a remote collection automatically.")
		}
		return 1
	}
	return code
}

func executeSources(ctx context.Context, opts sourceOptions, stdin io.Reader, service sources.Service) (*sourceOutput, []string, error) {
	out := &sourceOutput{}
	gaps := []string{}
	if opts.command == "scout" && !opts.scout || opts.command == "approve" && !opts.approve {
		return out, gaps, sources.ErrApprovalRequired
	}
	root, err := store.ResolveRoot(ctx, opts.repo)
	if err != nil {
		return out, gaps, err
	}
	switch opts.command {
	case "list":
		registry, loadErr := sources.Load(ctx, root)
		missing := errors.Is(loadErr, sources.ErrNoRegistry)
		if missing {
			registry = sources.Registry{Version: 1, Sources: []sources.ApprovedSource{}}
			loadErr = nil
			gaps = append(gaps, "Ask about known sources and obtain explicit approval before scouting.")
		}
		out.Result = struct {
			Registry           sources.Registry `json:"registry"`
			NeedsKnownSources  bool             `json:"needs_known_sources"`
			NeedsScoutApproval bool             `json:"needs_scout_approval"`
		}{registry, missing, missing}
		return out, gaps, loadErr
	case "scout":
		report, scoutErr := service.Scout(ctx, root, sources.ScoutOptions{Approved: opts.scout, KnownFile: opts.known, Context: opts.context, Timeout: opts.timeout})
		out.Result = report
		for _, gap := range report.Gaps {
			gaps = append(gaps, gap.Reason+" "+gap.Impact)
		}
		return out, gaps, scoutErr
	case "approve":
		raw, readErr := readSourceInput(opts.file, stdin, 1<<20)
		if readErr != nil {
			return out, gaps, readErr
		}
		selected, decodeErr := sources.DecodeSelection(raw)
		if decodeErr != nil {
			return out, gaps, decodeErr
		}
		registry, approveErr := sources.Approve(ctx, root, sources.ApprovalRequest{Approved: opts.approve, Sources: selected})
		out.Result = registry
		out.Saved = approveErr == nil
		return out, gaps, approveErr
	case "call":
		return collectSource(ctx, root, opts, stdin, service)
	}
	return out, gaps, errors.New("unknown sources command")
}

func readSourceInput(path string, stdin io.Reader, limit int) ([]byte, error) {
	reader := stdin
	if path != "-" {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("source input must be an existing regular file")
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, errors.New("source input unavailable")
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return nil, errors.New("source input changed while opening")
		}
		reader = file
	}
	if reader == nil {
		return nil, errors.New("source input unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil || len(raw) > limit {
		return nil, errors.New("source input exceeds limit or cannot be read")
	}
	return raw, nil
}

var sourceLedgerID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func collectSource(ctx context.Context, root string, opts sourceOptions, stdin io.Reader, service sources.Service) (*sourceOutput, []string, error) {
	out := &sourceOutput{}
	gaps := []string{}
	if !sourceLedgerID.MatchString(opts.run) || !sourceLedgerID.MatchString(opts.id) {
		return out, gaps, errors.New("invalid source evidence run or entry ID")
	}
	out.RunID, out.EntryID = opts.run, opts.id
	raw, err := readSourceInput(opts.args, stdin, ledger.MaxPayloadBytes)
	if err != nil {
		return out, gaps, err
	}
	callOpts := sources.CallOptions{SourceID: opts.source, Tool: opts.tool, Arguments: raw, Timeout: opts.timeout}
	if err := sources.ValidateCallOptions(callOpts); err != nil {
		return out, gaps, err
	}
	identity, _ := json.Marshal(struct {
		Source, Tool string
		Arguments    json.RawMessage
	}{opts.source, opts.tool, raw})
	if identity == nil {
		return out, gaps, errors.New("invalid source argument JSON")
	}
	digest := sha256.Sum256(identity)
	prefix := "Source collection request SHA256 " + hex.EncodeToString(digest[:]) + "; "
	db, err := store.OpenReadOnly(ctx, root)
	if err != nil {
		return out, gaps, err
	}
	run, loadErr := ledger.New(db, nil).Load(ctx, opts.run)
	_ = db.Close()
	if loadErr != nil {
		return out, gaps, loadErr
	}
	for _, entry := range run.Entries {
		if entry.ID != opts.id {
			continue
		}
		if entry.Kind != ledger.KindEvidence {
			return out, gaps, ledger.ErrConflict
		}
		payload, _ := ledger.DecodePayload(entry.Kind, entry.Payload)
		evidence := payload.(ledger.Evidence)
		if !strings.HasPrefix(evidence.Fact, prefix) {
			return out, gaps, ledger.ErrConflict
		}
		out.Committed, out.Replayed, out.Seq = true, true, entry.Seq
		gaps = append(gaps, "Replayed stored evidence receipt; remote call was not repeated and raw response is not stored.")
		if evidence.Gap != "" {
			gaps = append(gaps, evidence.Gap)
			return out, gaps, errors.New("previous source collection recorded a gap")
		}
		return out, gaps, nil
	}
	result, callErr := service.Call(ctx, root, callOpts)
	out.Result = result
	for _, gap := range result.Gaps {
		gaps = append(gaps, gap.Reason+" "+gap.Impact)
	}
	status := "collected"
	if callErr != nil {
		status = "failed"
	}
	evidence := ledger.Evidence{Fact: prefix + status + " from " + opts.source + "/" + opts.tool, Source: result.Reference, Gap: strings.Join(gaps, " ")}
	if evidence.Source == "" {
		evidence.Source = opts.source + "/" + opts.tool
	}
	if !result.At.IsZero() {
		evidence.ObservedAt = &result.At
	}
	payload, _ := json.Marshal(evidence)
	writer, writeErr := store.Open(ctx, root)
	if writeErr == nil {
		var entry ledger.Entry
		entry, writeErr = ledger.New(writer, service.Now).Add(ctx, ledger.AddRequest{RunID: opts.run, ID: opts.id, Kind: ledger.KindEvidence, Payload: payload})
		_ = writer.Close()
		if writeErr == nil {
			out.Committed, out.Seq = true, entry.Seq
		}
	}
	if writeErr != nil {
		gaps = append(gaps, "Collection returned but evidence was not recorded. Do not repeat the remote call automatically.")
		return out, gaps, errors.New("source evidence ledger write failed")
	}
	return out, gaps, callErr
}
