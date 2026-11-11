package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

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
	format, command, err := parse(args)
	var data any
	if err == nil {
		data, err = dispatch(command)
	}
	envelope := Envelope{OK: err == nil, Data: data, Gaps: []string{}, Errors: []string{}}
	code := 0
	if err != nil {
		envelope.Errors = append(envelope.Errors, safety.NewRedactor().Redact(err.Error()))
		code = 1
	}
	var writeErr error
	if format == "text" {
		if err != nil {
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

func parse(args []string) (string, string, error) {
	format := "json"
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--format":
			i++
			if i == len(args) {
				return "json", "", errors.New("--format requires json or text")
			}
			format = args[i]
		case strings.HasPrefix(arg, "--format="):
			format = strings.TrimPrefix(arg, "--format=")
		case strings.HasPrefix(arg, "-"):
			return "json", "", errors.New("unknown flag; usage: sre version [--format json|text]")
		default:
			positional = append(positional, arg)
		}
	}
	if format != "json" && format != "text" {
		return "json", "", errors.New("--format requires json or text")
	}
	if len(positional) != 1 || positional[0] != "version" {
		return format, "", errors.New("usage: sre version [--format json|text]")
	}
	return format, positional[0], nil
}
