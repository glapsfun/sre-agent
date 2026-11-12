package processlog

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/store"
)

type Filter struct {
	Slowest                 int
	Blocked, ByInvestigator bool
}
type Investigator struct {
	Agent     string `json:"agent"`
	Calls     int    `json:"calls"`
	Blocked   int    `json:"blocked"`
	Unknown   int    `json:"unknown"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
	Conflicts int    `json:"conflicts"`
}
type Report struct {
	RunID         string         `json:"run_id"`
	Calls         []Call         `json:"calls"`
	Investigators []Investigator `json:"investigators,omitempty"`
}

// Query aggregates the selected rows after filtering and limiting. Unknown
// durations sort after every measured duration, including measured zero.
func Query(ctx context.Context, s *store.Store, runID string, filter Filter) (Report, error) {
	if filter.Slowest < 0 {
		return Report{}, errors.New("slowest must be a positive integer")
	}
	if _, err := ledger.New(s, nil).Load(ctx, runID); err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			return Report{}, fmt.Errorf("unknown run: %w", ledger.ErrNotFound)
		}
		return Report{}, err
	}
	query := "SELECT " + columns + " FROM commands WHERE run_id=?"
	args := []any{runID}
	if filter.Blocked {
		query += " AND guard_decision='blocked'"
	}
	query += " ORDER BY "
	if filter.Slowest > 0 {
		query += "duration_ms IS NULL, duration_ms DESC, "
	}
	query += "recorded_at, session_id, tool_use_id"
	if filter.Slowest > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Slowest)
	}
	rows, err := s.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return Report{}, storageError(ctx, err)
	}
	defer rows.Close()
	r := Report{RunID: runID, Calls: []Call{}}
	for rows.Next() {
		c, err := scanCall(rows)
		if err != nil {
			return Report{}, storageError(ctx, err)
		}
		r.Calls = append(r.Calls, c)
	}
	if err := rows.Err(); err != nil {
		return Report{}, storageError(ctx, err)
	}
	if filter.ByInvestigator {
		groups := map[string]*Investigator{}
		for _, c := range r.Calls {
			group := groups[c.Agent]
			if group == nil {
				group = &Investigator{Agent: c.Agent}
				groups[c.Agent] = group
			}
			group.Calls++
			if c.GuardDecision != nil && *c.GuardDecision == "blocked" {
				group.Blocked++
			}
			switch c.Outcome {
			case "unknown":
				group.Unknown++
			case "succeeded":
				group.Succeeded++
			case "failed":
				group.Failed++
			case "conflict":
				group.Conflicts++
			}
		}
		for _, group := range groups {
			r.Investigators = append(r.Investigators, *group)
		}
		sort.Slice(r.Investigators, func(i, j int) bool { return r.Investigators[i].Agent < r.Investigators[j].Agent })
	}
	return r, nil
}
