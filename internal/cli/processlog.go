package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/glapsfun/sre-agent/internal/processlog"
	"github.com/glapsfun/sre-agent/internal/store"
)

type processLogOptions struct {
	repo, runID string
	filter      processlog.Filter
}

func executeProcessLog(ctx context.Context, opts processLogOptions) (processlog.Report, error) {
	root, err := store.ResolveRoot(ctx, opts.repo)
	if err != nil {
		return processlog.Report{}, err
	}
	db, err := store.Open(ctx, root)
	if err != nil {
		return processlog.Report{}, err
	}
	defer db.Close()
	return processlog.Query(ctx, db, opts.runID, opts.filter)
}

func writeProcessLogText(out io.Writer, report processlog.Report) error {
	if _, err := fmt.Fprintf(out, "Run %s: %d observed calls\n", report.RunID, len(report.Calls)); err != nil {
		return err
	}
	metric := func(value *int64) string {
		if value == nil {
			return "unknown"
		}
		return strconv.FormatInt(*value, 10)
	}
	for _, c := range report.Calls {
		decision := "unknown"
		if c.GuardDecision != nil {
			decision = *c.GuardDecision
		}
		if _, err := fmt.Fprintf(out, "%s session=%q call=%q agent=%q tool=%q guard=%s outcome=%s exit_status=%s duration_ms=%s command=%q path=%q\n", c.RecordedAt.Format("2006-01-02T15:04:05.999999999Z07:00"), c.SessionID, c.ToolUseID, c.Agent, c.Tool, decision, c.Outcome, metric(c.ExitStatus), metric(c.DurationMS), c.Command, c.Path); err != nil {
			return err
		}
	}
	for _, group := range report.Investigators {
		if _, err := fmt.Fprintf(out, "Investigator %q: calls=%d blocked=%d unknown=%d succeeded=%d failed=%d conflicts=%d\n", group.Agent, group.Calls, group.Blocked, group.Unknown, group.Succeeded, group.Failed, group.Conflicts); err != nil {
			return err
		}
	}
	return nil
}
