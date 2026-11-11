package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/discover"

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
	return run(args, stdout, stderr, func(command string) (any, error) {
		return versionData{Version: Version}, nil
	})
}

func run(args []string, stdout, stderr io.Writer, dispatch func(string) (any, error)) int {
	format, command, opts, err := parse(args)
	var data any
	if err == nil {
		if command == "discover" {
			data, err = discover.Discover(context.Background(), opts)
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
		if command == "discover" && data != nil {
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

func parse(args []string) (string, string, discover.Options, error) {
	format := "json"
	opts := discover.Options{}
	var positional []string
	discoveryFlag := false
	invalid := func(message string) (string, string, discover.Options, error) {
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
		case "--format", "--context", "--only", "--timeout":
		default:
			return invalid("unknown flag; usage: sre version|discover [--format json|text]")
		}
		if !hasValue {
			i++
			if i == len(args) {
				return invalid(name + " requires a value")
			}
			value = args[i]
		}
		if name != "--format" {
			discoveryFlag = true
		}
		switch name {
		case "--format":
			format = value
		case "--context":
			if strings.TrimSpace(value) == "" {
				return invalid("context must not be empty")
			}
			opts.Context = value
		case "--only":
			opts.Only = strings.Split(value, ",")
		case "--timeout":
			timeout, err := time.ParseDuration(value)
			if err != nil || timeout <= 0 {
				return invalid("timeout must be a positive duration")
			}
			opts.Timeout = timeout
		}
	}
	if format != "json" && format != "text" {
		return invalid("--format requires json or text")
	}
	if len(positional) != 1 || (positional[0] != "version" && positional[0] != "discover") {
		return format, "", opts, errors.New("usage: sre version|discover [--format json|text]")
	}
	if positional[0] == "version" && discoveryFlag {
		return invalid("discovery flags require the discover command")
	}
	if err := discover.Validate(opts); err != nil {
		return format, "", opts, err
	}
	return format, positional[0], opts, nil
}
