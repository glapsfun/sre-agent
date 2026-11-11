package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/prom"
)

func TestPromCLIJSONTextAndDefaultOptions(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		q := r.URL.Query()
		if q.Get("query") != "up" || q.Get("step") != "30" || q.Get("timeout") != "30" {
			t.Errorf("query: %v", q)
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"app","token":"hidden"},"values":[]}]}}`)
	}))
	defer server.Close()
	for _, format := range []string{"json", "text"} {
		var out, stderr bytes.Buffer
		code := RunWithInput([]string{"prom", "query", "up", "--url", server.URL, "--context", "unused", "--format", format}, strings.NewReader(""), &out, &stderr)
		if code != 0 || stderr.Len() != 0 || strings.Contains(out.String(), "hidden") || !strings.Contains(out.String(), "BEGIN EXTERNAL DATA") {
			t.Fatalf("code %d: %s %s", code, &out, &stderr)
		}
		if format == "json" {
			var envelope struct {
				OK   bool        `json:"ok"`
				Data prom.Report `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if !envelope.OK || len(envelope.Data.Series) != 1 || envelope.Data.Series[0].MissingSteps != 121 || envelope.Data.End.Sub(envelope.Data.Start) != time.Hour {
				t.Fatalf("%s", &out)
			}
		} else if !strings.Contains(out.String(), "Omitted series: 0") || !strings.Contains(out.String(), "missing steps: 121") {
			t.Fatalf("text: %s", &out)
		}
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestPromCLIValidationAndErrors(t *testing.T) {
	for _, args := range [][]string{
		{"prom", "query"}, {"prom", "query", ""}, {"prom", "query", "up", "extra"},
		{"prom", "query", "up", "--range", "0s"}, {"prom", "query", "up", "--step", "-1s"}, {"prom", "query", "up", "--top", "0"}, {"prom", "query", "up", "--timeout", "0s"},
		{"prom", "query", "up", "--only", "obs"}, {"prom", "query", "up", "--pod", "app"},
		{"version", "--url", "http://localhost:9090"}, {"discover", "--top", "2"}, {"logs", "fold", "--range", "1h"},
	} {
		var out, stderr bytes.Buffer
		if code := RunWithInput(args, strings.NewReader(""), &out, &stderr); code == 0 || !strings.Contains(out.String(), `"ok":false`) {
			t.Fatalf("accepted %v: %s", args, &out)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"invalid query token=hidden"}`)
	}))
	defer server.Close()
	var out, stderr bytes.Buffer
	if code := Run([]string{"prom", "query", "broken", "--url", server.URL}, &out, &stderr); code == 0 || !strings.Contains(out.String(), "invalid query") || strings.Contains(out.String(), "hidden") {
		t.Fatalf("%d %s", code, &out)
	}
}

func TestPromCLIRejectsSubmillisecondGridBeforeRequest(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer server.Close()
	for _, tc := range []struct{ flag, value string }{{"--range", "1.0001s"}, {"--step", "1.5ms"}} {
		for _, urlArgs := range [][]string{{"--url", server.URL}, nil} {
			var out, stderr bytes.Buffer
			code := Run(append([]string{"prom", "query", "up", tc.flag, tc.value}, urlArgs...), &out, &stderr)
			if code == 0 || !strings.Contains(out.String(), "whole milliseconds") || hits != 0 {
				t.Fatalf("code=%d hits=%d output=%s", code, hits, &out)
			}
		}
	}
}
