package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/glapsfun/sre-agent/internal/executil"
	"strings"
	"testing"
	"time"
)

func TestVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"version", "--format", "json"}, {"--format=json", "version"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		var envelope struct {
			OK   bool `json:"ok"`
			Data struct {
				Version string `json:"version"`
			} `json:"data"`
			Gaps   []string `json:"gaps"`
			Errors []string `json:"errors"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if !envelope.OK || envelope.Data.Version != Version || envelope.Gaps == nil || envelope.Errors == nil {
			t.Fatalf("invalid envelope: %s", out.String())
		}
		if errOut.Len() != 0 {
			t.Fatalf("unexpected stderr: %s", errOut.String())
		}
	}
	var out bytes.Buffer
	if Run([]string{"version", "--format=text"}, &out, &out) != 0 || out.String() != "sre "+Version+"\n" {
		t.Fatalf("text: %q", out.String())
	}
}

func TestCommandTimeoutEnvelope(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"version"}, &out, &errOut, func(string) (any, error) {
		result := (executil.Runner{Timeout: 20 * time.Millisecond}).Run(context.Background(), "sh", "-c", "sleep 2")
		return nil, result.Err
	})
	var envelope struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if code == 0 || envelope.OK || len(envelope.Errors) == 0 || !strings.Contains(envelope.Errors[0], "deadline exceeded") {
		t.Fatalf("timeout not reported: exit=%d %s", code, out.String())
	}
}

func TestInvalidArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"exec", "sh"}, {"version", "extra"}, {"version", "--bad"}, {"version", "--format"}, {"version", "--format=yaml"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut); code == 0 {
			t.Fatalf("accepted %q", args)
		}
		var envelope struct {
			OK     bool     `json:"ok"`
			Errors []string `json:"errors"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
			t.Fatalf("%q: %v (%s)", args, err, out.String())
		}
		if envelope.OK || len(envelope.Errors) == 0 {
			t.Fatalf("missing error: %s", out.String())
		}
	}
	var out bytes.Buffer
	Run([]string{"token=abc-secret"}, &out, &out)
	if strings.Contains(out.String(), "abc-secret") {
		t.Fatal("echoed untrusted argument")
	}
}
