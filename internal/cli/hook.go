package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/glapsfun/sre-agent/internal/guard"
	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/processlog"
	"github.com/glapsfun/sre-agent/internal/store"
)

var investigatorField = regexp.MustCompile(`"agent_type"[[:space:]\p{Z}]*:[[:space:]\p{Z}]*"(?:[^"]*:)?sre-(?:k8s-investigator|metrics-analyst|logs-investigator|change-historian|trace-analyst|eks-investigator|gke-investigator)"`)

func runHookPre(stdin io.Reader, stderr io.Writer) int {
	data, err := io.ReadAll(stdin)
	var fields map[string]json.RawMessage
	decodeErr := json.Unmarshal(data, &fields)
	if err != nil || decodeErr != nil {
		if investigatorField.Match(data) {
			return blockHook(stderr, "the guard couldn't read the hook event")
		}
		return 0
	}
	var agent string
	if json.Unmarshal(fields["agent_type"], &agent) != nil || !guard.IsInvestigator(agent) {
		return 0
	}
	var tool string
	if json.Unmarshal(fields["tool_name"], &tool) != nil {
		return blockHook(stderr, "the guard couldn't read the tool name")
	}
	decision := guard.Check(guard.Event{Agent: agent, Tool: tool, Input: fields["tool_input"]})
	logHook(fields, agent, tool, &decision, stderr)
	if decision.Block {
		return blockHook(stderr, decision.Reason)
	}
	return 0
}

func blockHook(stderr io.Writer, reason string) int {
	fmt.Fprintf(stderr, "sre-agent read-only guard blocked this: %s. Record it under GAPS and move on. Don't look for another way to run it.\n", reason)
	return 2
}

func runHookPost(stdin io.Reader, stderr io.Writer) int {
	data, err := io.ReadAll(stdin)
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(data, &fields) != nil {
		if investigatorField.Match(data) {
			fmt.Fprintln(stderr, "sre-agent process log: invalid hook event")
		}
		return 0
	}
	var agent, tool string
	if json.Unmarshal(fields["agent_type"], &agent) != nil || !guard.IsInvestigator(agent) {
		return 0
	}
	if json.Unmarshal(fields["tool_name"], &tool) != nil || tool == "" {
		fmt.Fprintln(stderr, "sre-agent process log: invalid hook metadata")
		return 0
	}
	logHook(fields, agent, tool, nil, stderr)
	return 0
}

func logHook(fields map[string]json.RawMessage, agent, tool string, decision *guard.Decision, stderr io.Writer) {
	// Minimal legacy guard fixtures predate call identity. Partial real metadata
	// is an error; only complete absence retains the guard-only contract.
	_, session := fields["session_id"]
	_, call := fields["tool_use_id"]
	_, cwd := fields["cwd"]
	if decision != nil && !session && !call && !cwd {
		return
	}
	if err := recordHook(fields, agent, tool, decision); err != nil {
		fmt.Fprintln(stderr, "sre-agent process log:", err)
	}
}

func recordHook(fields map[string]json.RawMessage, agent, tool string, decision *guard.Decision) error {
	invalid := errors.New("invalid hook metadata")
	e := processlog.Entry{RecordedAt: time.Now().UTC(), Agent: agent, Tool: tool}
	var cwd, event string
	for key, dest := range map[string]*string{"session_id": &e.SessionID, "tool_use_id": &e.ToolUseID, "cwd": &cwd, "hook_event_name": &event} {
		if json.Unmarshal(fields[key], dest) != nil || *dest == "" {
			return invalid
		}
	}
	if !filepath.IsAbs(cwd) {
		return invalid
	}
	if decision != nil {
		if event != "PreToolUse" {
			return invalid
		}
		e.Event = "pre"
		value := "allowed"
		if decision.Block {
			value = "blocked"
		}
		e.GuardDecision = &value
	} else {
		switch event {
		case "PostToolUse":
			e.Event = "success"
		case "PostToolUseFailure":
			e.Event = "failure"
		default:
			return invalid
		}
		if raw, ok := fields["duration_ms"]; ok {
			if json.Unmarshal(raw, &e.DurationMS) != nil || e.DurationMS != nil && *e.DurationMS < 0 {
				return invalid
			}
		}
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(fields["tool_input"], &input); err != nil {
		if decision == nil || !decision.Block {
			return invalid
		}
	} else {
		var raw json.RawMessage
		var dest *string
		switch tool {
		case "Bash":
			raw = input["command"]
			dest = &e.Command
		case "Read":
			raw = input["file_path"]
			dest = &e.Path
		case "Grep":
			raw = input["path"]
			dest = &e.Path
		}
		if dest != nil && len(raw) > 0 && json.Unmarshal(raw, dest) != nil {
			if decision == nil || !decision.Block {
				return invalid
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	root, err := store.ResolveRoot(ctx, cwd)
	if err != nil {
		return errors.New("repository unavailable")
	}
	db, err := store.Open(ctx, root)
	if err != nil {
		return errors.New("storage unavailable")
	}
	defer db.Close()
	service := ledger.New(db, nil)
	var run ledger.Run
	if explicit := os.Getenv("SRE_RUN_ID"); explicit != "" {
		run, err = service.Load(ctx, explicit)
	} else {
		run, err = service.LookupSession(ctx, e.SessionID)
	}
	if err != nil {
		return errors.New("run association unavailable; set a valid SRE_RUN_ID or bind this session")
	}
	e.RunID = run.ID
	if err := processlog.Record(ctx, db, e); err != nil {
		if errors.Is(err, processlog.ErrConflict) {
			return errors.New("call conflict; recorded identity and run retained")
		}
		return errors.New("recording failed")
	}
	return nil
}
