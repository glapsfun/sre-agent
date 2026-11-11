package timeline

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
)

type Runner interface {
	Run(context.Context, string, ...string) executil.Result
	Stream(context.Context, io.Writer, string, ...string) executil.Result
}

type KubeRunner interface {
	Run(context.Context, ...string) executil.Result
	Stream(context.Context, io.Writer, ...string) executil.Result
	Context() string
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Dependencies struct {
	Runner    Runner
	Kube      KubeRunner
	KubeError error
	HTTP      HTTPDoer
	Now       func() time.Time
}

type Options struct {
	Context, Namespace, Workload, Repo, AlertmanagerURL string
	Since, Timeout                                      time.Duration
}

type Object struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

type Entry struct {
	ID        string         `json:"id"`
	Time      time.Time      `json:"time"`
	TimeBasis string         `json:"time_basis"`
	Source    string         `json:"source"`
	Object    Object         `json:"object"`
	Change    string         `json:"change"`
	Command   string         `json:"command"`
	Details   map[string]any `json:"details"`
}

type Gap struct {
	Source  string `json:"source"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Count   int    `json:"count"`
}

type SourceSummary struct {
	Source   string         `json:"source"`
	Status   string         `json:"status"`
	Observed int            `json:"observed"`
	Matched  int            `json:"matched"`
	Context  map[string]any `json:"context"`
}

type SourceResult struct {
	Summary SourceSummary `json:"summary"`
	Entries []Entry       `json:"entries"`
	Gaps    []Gap         `json:"gaps"`
	Errors  []string      `json:"errors"`
}

type Report struct {
	Start     time.Time       `json:"start"`
	End       time.Time       `json:"end"`
	Context   string          `json:"context"`
	Namespace string          `json:"namespace"`
	Workload  string          `json:"workload"`
	Entries   []Entry         `json:"entries"`
	Sources   []SourceSummary `json:"sources"`
	Gaps      []Gap           `json:"gaps"`
	Errors    []string        `json:"errors"`
}

type Scope struct {
	Start, End                                          time.Time
	Context, Namespace, Workload, Repo, AlertmanagerURL string
	Target                                              *Object
	Objects                                             map[string]Object
	Related                                             map[string]bool
}
