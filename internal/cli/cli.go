package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/discover"
	"github.com/glapsfun/sre-agent/internal/logs"
	"github.com/glapsfun/sre-agent/internal/processlog"
	"github.com/glapsfun/sre-agent/internal/prom"
	"github.com/glapsfun/sre-agent/internal/safety"
	"github.com/glapsfun/sre-agent/internal/timeline"
)

var Version = "dev"

type Envelope struct {
	OK     bool     `json:"ok"`
	Data   any      `json:"data"`
	Gaps   []string `json:"gaps"`
	Errors []string `json:"errors"`
}

type versionData struct {
	Version string `json:"version"`
}

func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithInput(args, os.Stdin, stdout, stderr)
}

func RunWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithInput(args, stdin, stdout, stderr, func(command string) (any, error) {
		return versionData{Version: Version}, nil
	})
}

func run(args []string, stdout, stderr io.Writer, dispatch func(string) (any, error)) int {
	return runWithInput(args, os.Stdin, stdout, stderr, dispatch)
}

func runWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer, dispatch func(string) (any, error)) int {
	if len(args) == 2 && args[0] == "hook" && args[1] == "pre" {
		return runHookPre(stdin, stderr)
	}
	if len(args) == 2 && args[0] == "hook" && args[1] == "post" {
		return runHookPost(stdin, stderr)
	}
	format, command, opts, err := parse(args)
	var data any
	if err == nil {
		if command == "log" {
			data, err = executeProcessLog(context.Background(), opts.processlog)
		} else if command == "incident add" || command == "recall" {
			return runIncident(command, opts.incident, format, stdout, stderr)
		} else if isLedgerCommand(command) {
			return runLedger(command, opts.ledger, format, stdin, stdout, stderr)
		} else if command == "timeline" {
			var report timeline.Report
			report, err = timeline.Collect(context.Background(), opts.timeline, timeline.Dependencies{})
			if err == nil {
				return writeTimeline(format, report, stdout, stderr)
			}
		} else if command == "discover" {
			data, err = discover.Discover(context.Background(), opts.discovery)
		} else if command == "prom query" {
			var report prom.Report
			report, err = prom.Query(context.Background(), opts.prom)
			if err == nil {
				data, err = prom.Present(report)
			}
		} else if command == "logs fold" {
			var report logs.Report
			if opts.logs.Pod == "" {
				report, err = logs.Fold(stdin, opts.logs.Options)
			} else {
				report, err = logs.FoldPod(context.Background(), opts.logs)
			}
			if err == nil {
				data, err = logs.Present(report)
			}
		} else {
			data, err = dispatch(command)
		}
	}
	envelope := Envelope{OK: err == nil, Data: data, Gaps: []string{}, Errors: []string{}}
	code := 0
	if err != nil {
		envelope.Errors = append(envelope.Errors, safety.NewRedactor().Redact(err.Error()))
		code = 1
	}
	if report, ok := data.(discover.Report); ok {
		for _, name := range []string{"env", "obs", "aws", "gcloud", "github"} {
			probe, selected := report.Probes[name]
			if !selected {
				continue
			}
			for _, gap := range probe.Gaps {
				envelope.Gaps = append(envelope.Gaps, name+": "+gap)
			}
			if probe.Status == discover.Timeout || probe.Status == discover.Denied || probe.Status == discover.Error {
				envelope.Errors = append(envelope.Errors, name+": "+string(probe.Status))
			}
		}
		if len(envelope.Errors) > 0 {
			envelope.OK = false
			code = 1
		}
	}
	var writeErr error
	if format == "text" {
		if report, ok := data.(processlog.Report); ok && err == nil {
			writeErr = writeProcessLogText(stdout, report)
		} else if report, ok := data.(prom.Report); ok && err == nil {
			writeErr = writePromText(stdout, report)
		} else if report, ok := data.(logs.Report); ok && err == nil {
			writeErr = writeLogsText(stdout, report)
		} else if command == "discover" && data != nil {
			encoded, encodeErr := json.MarshalIndent(envelope.Data, "", "  ")
			if encodeErr != nil {
				writeErr = encodeErr
			} else {
				_, writeErr = fmt.Fprintln(stdout, string(encoded))
			}
		} else if err != nil {
			_, writeErr = fmt.Fprintln(stdout, "error:", envelope.Errors[0])
		} else {
			_, writeErr = fmt.Fprintln(stdout, "sre", Version)
		}
	} else {
		writeErr = json.NewEncoder(stdout).Encode(envelope)
	}
	if writeErr != nil {
		fmt.Fprintln(stderr, "failed to write command output")
		return 1
	}
	return code
}

func writeLogsText(out io.Writer, report logs.Report) error {
	if _, err := fmt.Fprintf(out, "Total lines: %d\nOmitted patterns: %d, omitted lines: %d\n", report.TotalLines, report.OmittedPatterns, report.OmittedLines); err != nil {
		return err
	}
	for _, pattern := range report.Patterns {
		first, last := "unknown", "unknown"
		if pattern.FirstSeen != nil {
			first = pattern.FirstSeen.Format(time.RFC3339Nano)
		}
		if pattern.LastSeen != nil {
			last = pattern.LastSeen.Format(time.RFC3339Nano)
		}
		level := pattern.Level
		if level == "" {
			level = "unknown"
		}
		if _, err := fmt.Fprintf(out, "\nCount: %d, level: %s, first: %s, last: %s\n%s%s", pattern.Count, level, first, last, pattern.Template, pattern.Sample); err != nil {
			return err
		}
	}
	return nil
}

type commandOptions struct {
	discovery  discover.Options
	logs       logs.PodOptions
	prom       prom.Options
	timeline   timeline.Options
	ledger     ledgerOptions
	processlog processLogOptions
	incident   incidentOptions
}

func parse(args []string) (string, string, commandOptions, error) {
	format := "json"
	opts := commandOptions{}
	var positional []string
	discoveryFlag, logFlag, podFlag, contextFlag := false, false, false, false
	promFlag, timeoutFlag := false, false
	topFlag, incidentFlag := false, false
	timelineFlag, namespaceFlag, sinceFlag := false, false, false
	repoFlag := false
	processLogFlag := false
	ledgerFlags := map[string]bool{}
	invalid := func(message string) (string, string, commandOptions, error) {
		return "json", "", opts, errors.New(message)
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || (len(positional) == 2 && positional[0] == "prom" && positional[1] == "query" && !strings.HasPrefix(arg, "--")) {
			positional = append(positional, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--format", "--context", "--only", "--timeout", "--max-patterns", "--pod", "--namespace", "--container", "--previous", "--since", "--url", "--range", "--step", "--top", "--workload", "--repo", "--alertmanager-url", "--id", "--session-id", "--run", "--file", "--offset", "--slowest", "--blocked", "--by-investigator", "--service", "--symptom", "--fingerprint":
		default:
			return invalid("unknown flag; usage: sre version|discover|logs fold|prom query '<PromQL>'|timeline|run|ledger|incident add|recall [--format json|text]")
		}
		if (name == "--previous" || name == "--blocked" || name == "--by-investigator") && !hasValue {
			value, hasValue = "true", true
		}
		if !hasValue {
			i++
			if i == len(args) {
				return invalid(name + " requires a value")
			}
			value = args[i]
		}
		switch name {
		case "--format":
			format = value
		case "--service", "--symptom", "--fingerprint":
			incidentFlag = true
			switch name {
			case "--service":
				opts.incident.service = value
			case "--symptom":
				opts.incident.symptom = value
			case "--fingerprint":
				opts.incident.fingerprint = value
			}
		case "--slowest", "--blocked", "--by-investigator":
			processLogFlag = true
			if name == "--slowest" {
				n, err := strconv.Atoi(value)
				if err != nil || n <= 0 {
					return invalid("slowest must be a positive integer")
				}
				opts.processlog.filter.Slowest = n
			} else {
				enabled, err := strconv.ParseBool(value)
				if err != nil {
					return invalid(name + " must be true or false")
				}
				if name == "--blocked" {
					opts.processlog.filter.Blocked = enabled
				} else {
					opts.processlog.filter.ByInvestigator = enabled
				}
			}
		case "--id", "--session-id", "--run", "--file", "--offset":
			ledgerFlags[name] = true
			if name == "--run" {
				opts.incident.runID = value
			}
			if err := opts.ledger.set(name, value); err != nil {
				return invalid(err.Error())
			}
		case "--context":
			if strings.TrimSpace(value) == "" {
				return invalid("context must not be empty")
			}
			contextFlag = true
			opts.discovery.Context, opts.logs.Context = value, value
			opts.prom.Context = value
			opts.timeline.Context = value
		case "--only":
			discoveryFlag = true
			opts.discovery.Only = strings.Split(value, ",")
		case "--timeout":
			timeoutFlag = true
			timeout, err := time.ParseDuration(value)
			if err != nil || timeout <= 0 {
				return invalid("timeout must be a positive duration")
			}
			opts.discovery.Timeout = timeout
			opts.prom.Timeout = timeout
			opts.timeline.Timeout = timeout
		case "--url":
			promFlag = true
			if value == "" {
				return invalid("url must not be empty")
			}
			opts.prom.URL = value
		case "--range", "--step":
			promFlag = true
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 {
				return invalid(name + " must be a positive duration")
			}
			if name == "--range" {
				opts.prom.Range = duration
			} else {
				opts.prom.Step = duration
			}
		case "--top":
			topFlag = true
			top, err := strconv.Atoi(value)
			if err != nil || top <= 0 {
				return invalid("top must be a positive integer")
			}
			opts.prom.Top = top
			opts.incident.top = top
		case "--max-patterns":
			logFlag = true
			maximum, err := strconv.Atoi(value)
			if err != nil || maximum <= 0 {
				return invalid("max-patterns must be a positive integer")
			}
			opts.logs.Options.MaxPatterns = maximum
		case "--workload", "--repo", "--alertmanager-url":
			if name == "--repo" {
				repoFlag = true
			} else {
				timelineFlag = true
			}
			if strings.TrimSpace(value) == "" {
				return invalid(name + " must not be empty")
			}
			switch name {
			case "--workload":
				opts.timeline.Workload = value
			case "--repo":
				opts.timeline.Repo = value
				opts.ledger.repo = value
				opts.processlog.repo = value
				opts.incident.repo = value
			case "--alertmanager-url":
				opts.timeline.AlertmanagerURL = value
			}
		case "--pod", "--namespace", "--container":
			if name == "--namespace" {
				namespaceFlag = true
			} else {
				podFlag = true
			}
			if value == "" {
				return invalid(name + " must not be empty")
			}
			switch name {
			case "--pod":
				opts.logs.Pod = value
			case "--namespace":
				opts.logs.Namespace = value
				opts.timeline.Namespace = value
			case "--container":
				opts.logs.Container = value
			}
		case "--previous":
			podFlag = true
			previous, err := strconv.ParseBool(value)
			if err != nil {
				return invalid("previous must be true or false")
			}
			opts.logs.Previous = previous
		case "--since":
			sinceFlag = true
			since, err := time.ParseDuration(value)
			if err != nil || since <= 0 {
				return invalid("since must be a positive duration")
			}
			opts.logs.Since = since
			opts.timeline.Since = since
		}
	}
	if format != "json" && format != "text" {
		return invalid("--format requires json or text")
	}
	command := strings.Join(positional, " ")
	if len(positional) == 3 && positional[0] == "prom" && positional[1] == "query" {
		command = "prom query"
		opts.prom.Query = positional[2]
	}
	if command == "incident add" || command == "recall" {
		if discoveryFlag || logFlag || podFlag || contextFlag || promFlag || timeoutFlag || timelineFlag || namespaceFlag || sinceFlag || processLogFlag {
			return invalid("command flags do not apply to incident commands")
		}
		for flag := range ledgerFlags {
			if command != "incident add" || flag != "--run" {
				return invalid("flag does not apply to this incident command")
			}
		}
		if command == "incident add" {
			if strings.TrimSpace(opts.incident.runID) == "" {
				return invalid("incident capture requires explicit --run")
			}
			if topFlag {
				return invalid("--top requires recall or prom query")
			}
		}
		return format, command, opts, nil
	}
	if incidentFlag {
		return invalid("signature flags require incident add or recall")
	}
	promFlag = promFlag || topFlag
	if len(positional) > 0 && positional[0] == "log" {
		if len(positional) != 2 {
			return invalid("usage: sre log RUN_ID [--slowest N] [--blocked] [--by-investigator]")
		}
		if discoveryFlag || logFlag || podFlag || contextFlag || promFlag || timeoutFlag || timelineFlag || namespaceFlag || sinceFlag || len(ledgerFlags) > 0 {
			return invalid("command flags do not apply to log")
		}
		opts.processlog.runID = positional[1]
		return format, "log", opts, nil
	}
	if processLogFlag {
		return invalid("process log flags require log")
	}
	if len(positional) >= 2 && (positional[0] == "run" || positional[0] == "ledger") {
		command = strings.Join(positional[:2], " ")
		if !isLedgerCommand(command) {
			return invalid("unknown ledger command")
		}
		if discoveryFlag || logFlag || podFlag || contextFlag || promFlag || timeoutFlag || timelineFlag || namespaceFlag || sinceFlag {
			return invalid("command flags do not apply to ledger commands")
		}
		if err := opts.ledger.validate(command, positional[2:], ledgerFlags); err != nil {
			return invalid(err.Error())
		}
		return format, command, opts, nil
	}
	if len(ledgerFlags) > 0 {
		return invalid("ledger flags require run or ledger commands")
	}
	timelineFlag = timelineFlag || repoFlag
	if command != "timeline" {
		if timelineFlag {
			return invalid("timeline flags require timeline")
		}
		podFlag = podFlag || namespaceFlag || sinceFlag
	}
	switch command {
	case "timeline":
		if discoveryFlag || logFlag || podFlag || promFlag {
			return invalid("command flags do not apply to timeline")
		}
		if err := timeline.Validate(opts.timeline); err != nil {
			return format, "", opts, err
		}
	case "version":
		if discoveryFlag || logFlag || podFlag || contextFlag || promFlag || timeoutFlag {
			return invalid("command flags require discover or logs fold")
		}
	case "discover":
		if logFlag || podFlag || promFlag {
			return invalid("command flags require logs fold or prom query")
		}
		if err := discover.Validate(opts.discovery); err != nil {
			return format, "", opts, err
		}
	case "logs fold":
		if discoveryFlag || timeoutFlag || promFlag {
			return invalid("discovery flags require discover")
		}
		if podFlag || contextFlag {
			if err := logs.ValidatePod(opts.logs); err != nil {
				return format, "", opts, err
			}
		}
	case "prom query":
		if discoveryFlag || logFlag || podFlag {
			return invalid("command flags require discover or logs fold")
		}
		if err := prom.Validate(opts.prom); err != nil {
			return format, "", opts, err
		}
	default:
		return format, "", opts, errors.New("usage: sre version|discover|logs fold|prom query '<PromQL>'|timeline|run|ledger|incident add|recall [--format json|text]")
	}
	return format, command, opts, nil
}
