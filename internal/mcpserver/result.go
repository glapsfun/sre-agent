package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/glapsfun/sre-agent/internal/ledger"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const resultBytes = 8000

type envelope struct {
	OK     bool     `json:"ok"`
	Data   any      `json:"data"`
	Gaps   []string `json:"gaps"`
	Errors []string `json:"errors"`
}

type ledgerOutput struct {
	RunID      string       `json:"run_id"`
	ThroughSeq int64        `json:"through_seq"`
	Committed  bool         `json:"committed,omitempty"`
	EntryID    string       `json:"entry_id,omitempty"`
	Seq        int64        `json:"seq,omitempty"`
	View       *ledger.View `json:"view,omitempty"`
}

func success(data any) envelope {
	return envelope{OK: true, Data: data, Gaps: []string{}, Errors: []string{}}
}

func failure(label string, err error) envelope {
	message := safety.NewRedactor().Redact(err.Error())
	if label == "ledger error" && len(message) > 512 {
		message = "ledger operation failed; inspect inputs and repository access"
	}
	wrapper, wrapErr := safety.NewWrapper()
	if wrapErr != nil {
		message = "operation failed"
	} else {
		message = wrapper.Wrap(label, message)
	}
	return envelope{Data: nil, Gaps: []string{}, Errors: []string{message}}
}

func boundary(label string) (string, string, error) {
	wrapper, err := safety.NewWrapper()
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(wrapper.Wrap(label, ""), "\n")
	return parts[0], parts[2], nil
}

func toolResult(env envelope) *mcp.CallToolResult {
	text, err := json.Marshal(env)
	if err != nil {
		return toolResult(failure("tool output error", fmt.Errorf("cannot encode tool output")))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}, StructuredContent: env, IsError: !env.OK}
}

func boundEncoded(encoded []byte, command string) []byte {
	size := len(encoded)
	if size <= resultBytes {
		return encoded
	}
	var wire map[string]json.RawMessage
	var original struct {
		Data json.RawMessage `json:"data"`
	}
	var receipt ledgerOutput
	_ = json.Unmarshal(encoded, &wire)
	_ = json.Unmarshal(wire["structuredContent"], &original)
	_ = json.Unmarshal(original.Data, &receipt)
	var env envelope
	if receipt.Committed {
		compact := receipt
		compact.View = nil
		env = success(&compact)
		env.Gaps = []string{fmt.Sprintf("Write committed; result_too_large: actual encoded bytes %d, limit %d. Retrieve the complete output with %s. Retry only with the same entry ID.", size, resultBytes, command)}
	} else {
		env = envelope{Data: struct {
			Code        string `json:"code"`
			ActualBytes int    `json:"actual_bytes"`
			Limit       int    `json:"limit"`
			CLI         string `json:"cli"`
		}{"result_too_large", size, resultBytes, command}, Gaps: []string{}, Errors: []string{"result_too_large: complete result omitted; use the CLI command in data.cli"}}
	}
	result := toolResult(env)
	wire["structuredContent"], _ = json.Marshal(env)
	wire["content"], _ = json.Marshal(result.Content)
	if result.IsError {
		wire["isError"] = json.RawMessage("true")
	} else {
		delete(wire, "isError")
	}
	bounded, _ := json.Marshal(wire)
	return bounded
}
