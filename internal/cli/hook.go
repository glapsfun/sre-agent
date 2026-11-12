package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"

	"github.com/glapsfun/sre-agent/internal/guard"
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
	if decision.Block {
		return blockHook(stderr, decision.Reason)
	}
	return 0
}

func blockHook(stderr io.Writer, reason string) int {
	fmt.Fprintf(stderr, "sre-agent read-only guard blocked this: %s. Record it under GAPS and move on. Don't look for another way to run it.\n", reason)
	return 2
}
