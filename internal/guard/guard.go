// Package guard checks investigator tool calls without executing commands or accessing storage.
package guard

import (
	"encoding/json"
	"strings"
)

type Event struct {
	Agent string
	Tool  string
	Input json.RawMessage
}

type Decision struct {
	Block  bool
	Reason string
}

func IsInvestigator(agent string) bool {
	return investigators[agent[strings.LastIndex(agent, ":")+1:]]
}

func Check(event Event) Decision {
	if !IsInvestigator(event.Agent) {
		return Decision{}
	}
	var reason string
	if strings.HasPrefix(event.Tool, "mcp__plugin_sre-agent_sre__") {
		switch strings.TrimPrefix(event.Tool, "mcp__plugin_sre-agent_sre__") {
		case "discover", "logs_fold", "prom_query", "timeline", "recall", "run_resume", "report":
			return Decision{}
		default:
			return Decision{Block: true, Reason: "the MCP operation isn't a read-only operation"}
		}
	}
	switch event.Tool {
	case "Bash":
		var input map[string]json.RawMessage
		var command *string
		if json.Unmarshal(event.Input, &input) != nil || json.Unmarshal(input["command"], &command) != nil || command == nil {
			reason = "the guard couldn't read the Bash command"
		} else {
			reason = checkCommandLine(*command)
		}
	case "Read", "Grep":
		var input map[string]json.RawMessage
		if len(event.Input) > 0 && json.Unmarshal(event.Input, &input) != nil {
			reason = "the guard couldn't read the tool input"
			break
		}
		field := "file_path"
		if event.Tool == "Grep" {
			field = "path"
		}
		var path string
		if raw, ok := input[field]; ok && json.Unmarshal(raw, &path) != nil {
			reason = "the guard couldn't read the tool path"
			break
		}
		if credentialPath.MatchString(path) {
			reason = "it reads a credential file"
		}
	}
	return Decision{Block: reason != "", Reason: reason}
}
