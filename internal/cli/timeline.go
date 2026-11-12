package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/timeline"
)

type timelineData struct {
	Begin string `json:"external_data_begin"`
	timeline.Report
	End string `json:"external_data_end"`
}

func writeTimeline(format string, report timeline.Report, stdout, stderr io.Writer) int {
	clean, err := timeline.RedactReport(context.Background(), report)
	if err != nil {
		fmt.Fprintln(stderr, "failed to prepare timeline output")
		return 1
	}
	wrapper, err := safety.NewWrapper()
	if err != nil {
		fmt.Fprintln(stderr, "failed to create timeline output boundary")
		return 1
	}
	code := 0
	if len(clean.Errors) > 0 {
		code = 1
	}
	if format == "text" {
		text, err := timeline.RenderText(clean, wrapper)
		if err == nil {
			_, err = io.WriteString(stdout, text)
		}
		if err != nil {
			fmt.Fprintln(stderr, "failed to write command output")
			return 1
		}
		return code
	}
	boundaries := strings.Split(wrapper.Wrap("timeline", ""), "\n")
	envelope := Envelope{OK: code == 0, Data: timelineData{Begin: boundaries[0], Report: clean, End: boundaries[2]}, Gaps: []string{}, Errors: []string{}}
	for _, gap := range clean.Gaps {
		envelope.Gaps = append(envelope.Gaps, wrapper.Wrap("timeline gap", gap.Source+": "+gap.Code+": "+gap.Message))
	}
	for _, message := range clean.Errors {
		envelope.Errors = append(envelope.Errors, wrapper.Wrap("timeline error", message))
	}
	if err := json.NewEncoder(stdout).Encode(envelope); err != nil {
		fmt.Fprintln(stderr, "failed to write command output")
		return 1
	}
	return code
}
