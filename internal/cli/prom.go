package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/prom"
)

func writePromText(out io.Writer, report prom.Report) error {
	var text strings.Builder
	fmt.Fprintf(&text, "Range: %s to %s, step: %gs\nOmitted series: %d\n%s\n", report.Start.Format(time.RFC3339Nano), report.End.Format(time.RFC3339Nano), report.StepSeconds, report.OmittedSeries, report.Begin)
	number := func(value *float64) string {
		if value == nil {
			return "null"
		}
		return strconv.FormatFloat(*value, 'g', -1, 64)
	}
	for _, series := range report.Series {
		labels, _ := json.Marshal(series.Labels)
		fmt.Fprintf(&text, "\nLabels: %s\nfirst: %s, last: %s, min: %s, max: %s, mean: %s\nmissing steps: %d, NaN: %d, +Inf: %d, -Inf: %d\n", labels, number(series.First), number(series.Last), number(series.Min), number(series.Max), number(series.Mean), series.MissingSteps, series.NaNCount, series.PositiveInfCount, series.NegativeInfCount)
		if series.Change == nil {
			fmt.Fprintln(&text, "change: none")
		} else {
			fmt.Fprintf(&text, "change: %s, before: %g, after: %g\n", series.Change.Time.Format(time.RFC3339Nano), series.Change.Before, series.Change.After)
		}
	}
	for _, info := range report.Infos {
		fmt.Fprintln(&text, "info:", info)
	}
	fmt.Fprintln(&text, report.Finish)
	_, err := io.WriteString(out, text.String())
	return err
}
