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
	"github.com/glapsfun/sre-agent/internal/safety"
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
	format, command, opts, err := parse(args)
	var data any
	if err == nil {
		if command == "discover" {
			data, err = discover.Discover(context.Background(), opts.discovery)
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
		if report, ok := data.(logs.Report); ok && err == nil {
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
	discovery discover.Options
	logs      logs.PodOptions
}

func parse(args []string) (string, string, commandOptions, error) {
	format := "json"
	opts := commandOptions{}
	var positional []string
	discoveryFlag, logFlag, podFlag, contextFlag := false, false, false, false
	invalid := func(message string) (string, string, commandOptions, error) {
		return "json", "", opts, errors.New(message)
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--format", "--context", "--only", "--timeout", "--max-patterns", "--pod", "--namespace", "--container", "--previous", "--since":
		default:
			return invalid("unknown flag; usage: sre version|discover|logs fold [--format json|text]")
		}
		if name == "--previous" && !hasValue {
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
		case "--context":
			if strings.TrimSpace(value) == "" {
				return invalid("context must not be empty")
			}
			contextFlag = true
			opts.discovery.Context, opts.logs.Context = value, value
		case "--only":
			discoveryFlag = true
			opts.discovery.Only = strings.Split(value, ",")
		case "--timeout":
			discoveryFlag = true
			timeout, err := time.ParseDuration(value)
			if err != nil || timeout <= 0 {
				return invalid("timeout must be a positive duration")
			}
			opts.discovery.Timeout = timeout
		case "--max-patterns":
			logFlag = true
			maximum, err := strconv.Atoi(value)
			if err != nil || maximum <= 0 {
				return invalid("max-patterns must be a positive integer")
			}
			opts.logs.Options.MaxPatterns = maximum
		case "--pod", "--namespace", "--container":
			podFlag = true
			if value == "" {
				return invalid(name + " must not be empty")
			}
			switch name {
			case "--pod":
				opts.logs.Pod = value
			case "--namespace":
				opts.logs.Namespace = value
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
			podFlag = true
			since, err := time.ParseDuration(value)
			if err != nil || since <= 0 {
				return invalid("since must be a positive duration")
			}
			opts.logs.Since = since
		}
	}
	if format != "json" && format != "text" {
		return invalid("--format requires json or text")
	}
	command := strings.Join(positional, " ")
	switch command {
	case "version":
		if discoveryFlag || logFlag || podFlag || contextFlag {
			return invalid("command flags require discover or logs fold")
		}
	case "discover":
		if logFlag || podFlag {
			return invalid("log flags require logs fold")
		}
		if err := discover.Validate(opts.discovery); err != nil {
			return format, "", opts, err
		}
	case "logs fold":
		if discoveryFlag {
			return invalid("discovery flags require discover")
		}
		if podFlag || contextFlag {
			if err := logs.ValidatePod(opts.logs); err != nil {
				return format, "", opts, err
			}
		}
	default:
		return format, "", opts, errors.New("usage: sre version|discover|logs fold [--format json|text]")
	}
	return format, command, opts, nil
}
