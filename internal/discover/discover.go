// Package discover provides read-only, bounded environment discovery.
package discover

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
	"github.com/glapsfun/sre-agent/internal/safety"
)

const DefaultTimeout = 10 * time.Second

type Options struct {
	Context     string
	Only        []string
	Timeout     time.Duration
	SkipCluster bool
}

type Runner interface {
	Run(context.Context, string, ...string) executil.Result
}

// Service allows tests and other transports to use the same discovery probes.
// Runner implementations must honor cancellation, as executil.Runner does.
type Service struct {
	Runner   Runner
	LookPath func(string) (string, error)
}

type Status string

const (
	OK      Status = "ok"
	Missing Status = "missing"
	Denied  Status = "denied"
	Timeout Status = "timeout"
	Error   Status = "error"
)

type Findings struct {
	Env    *Environment   `json:"env,omitempty"`
	Obs    *Observability `json:"obs,omitempty"`
	AWS    *AWS           `json:"aws,omitempty"`
	GCloud *GCloud        `json:"gcloud,omitempty"`
	GitHub *GitHub        `json:"github,omitempty"`
}

// The markers bracket structured findings without turning JSON into a string.
type ProbeResult struct {
	Status   Status   `json:"status"`
	Begin    string   `json:"begin"`
	Findings Findings `json:"findings"`
	End      string   `json:"end"`
	Gaps     []string `json:"gaps"`
}

type Report struct {
	Probes map[string]ProbeResult `json:"probes"`
}

var names = []string{"env", "obs", "aws", "gcloud", "github"}

func Validate(opts Options) error {
	if opts.Timeout < 0 {
		return errors.New("timeout must be positive")
	}
	if opts.Only != nil && len(opts.Only) == 0 {
		return errors.New("only must select at least one probe")
	}
	for _, name := range opts.Only {
		valid := false
		for _, known := range names {
			valid = valid || name == known
		}
		if !valid {
			return errors.New("only accepts env,obs,aws,gcloud,github")
		}
	}
	if strings.ContainsAny(opts.Context, "\r\n") {
		return errors.New("invalid Kubernetes context")
	}
	return nil
}

func Discover(ctx context.Context, opts Options) (Report, error) {
	return (Service{Runner: executil.Runner{}, LookPath: exec.LookPath}).Discover(ctx, opts)
}

func (s Service) Discover(ctx context.Context, opts Options) (Report, error) {
	report := Report{Probes: map[string]ProbeResult{}}
	if err := Validate(opts); err != nil {
		return report, err
	}
	if s.Runner == nil {
		s.Runner = executil.Runner{}
	}
	if s.LookPath == nil {
		s.LookPath = exec.LookPath
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	opts.SkipCluster = opts.SkipCluster || os.Getenv("SRE_SKIP_CLUSTER") == "true"
	if opts.Context == "" {
		opts.Context = strings.TrimSpace(os.Getenv("SRE_KUBE_CONTEXT"))
	}
	selected := opts.Only
	if selected == nil {
		selected = names
	}
	wrapper, err := safety.NewWrapper()
	if err != nil {
		return report, err
	}
	var pin contextPin
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]bool{}
	for _, name := range selected {
		if seen[name] {
			continue
		}
		seen[name] = true
		wg.Go(func() {
			probeCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
			defer cancel()
			p := probe{service: s, ctx: probeCtx, opts: opts, pin: &pin, result: ProbeResult{Status: OK, Gaps: []string{}}}
			switch name {
			case "env":
				p.environment()
			case "obs":
				p.observability()
			case "aws":
				p.aws()
			case "gcloud":
				p.gcloud()
			case "github":
				p.github()
			}
			if env := p.result.Findings.Env; env != nil {
				env.Namespaces = limit(&p, "namespaces", env.Namespaces, 100)
			}
			if obs := p.result.Findings.Obs; obs != nil {
				obs.Endpoints = limit(&p, "observability endpoints", obs.Endpoints, 20)
				obs.Ingresses = limit(&p, "observability ingresses", obs.Ingresses, 20)
			}
			if p.err() != nil {
				p.fail(Timeout, "probe deadline or cancellation reached")
			}
			boundary := strings.Split(wrapper.Wrap(name, ""), "\n")
			p.result.Begin, p.result.End = boundary[0], boundary[2]
			mu.Lock()
			report.Probes[name] = p.result
			mu.Unlock()
		})
	}
	wg.Wait()
	return report, nil
}

func limit[T any](p *probe, label string, items []T, maximum int) []T {
	if len(items) <= maximum {
		return items
	}
	p.result.Gaps = append(p.result.Gaps, fmt.Sprintf("%s: %d additional entries omitted from report", label, len(items)-maximum))
	return items[:maximum]
}

type contextPin struct {
	once   sync.Once
	value  string
	result executil.Result
}

type probe struct {
	service Service
	ctx     context.Context
	opts    Options
	pin     *contextPin
	result  ProbeResult
}

func (p *probe) err() error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	// Timer cancellation may not run before a ready command result is accepted.
	if deadline, ok := p.ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func (p *probe) fail(status Status, gap string) {
	priority := map[Status]int{OK: 0, Missing: 1, Denied: 2, Error: 3, Timeout: 4}
	if priority[status] > priority[p.result.Status] {
		p.result.Status = status
	}
	p.result.Gaps = append(p.result.Gaps, safety.NewRedactor().Redact(gap))
}

func (p *probe) have(name string) bool { _, err := p.service.LookPath(name); return err == nil }

func (p *probe) require(name string) bool {
	if p.have(name) {
		return true
	}
	p.fail(Missing, name+" CLI unavailable")
	return false
}

func (p *probe) online() bool {
	if !p.opts.SkipCluster {
		return true
	}
	p.fail(Missing, "live discovery skipped: SRE_SKIP_CLUSTER=true")
	return false
}

func (p *probe) check(label string, result executil.Result) bool {
	if result.Err == nil {
		result.Err = p.err()
	}
	if result.Err == nil {
		return true
	}
	status := Error
	for _, bound := range []error{safety.ErrResponseTooLarge, safety.ErrResponseComplexity} {
		if errors.Is(result.Err, bound) {
			p.fail(Error, label+": "+bound.Error())
			return false
		}
	}
	if errors.Is(result.Err, context.DeadlineExceeded) || errors.Is(result.Err, context.Canceled) {
		status = Timeout
	} else {
		message := strings.ToLower(result.Stderr + " " + result.Err.Error())
		for _, word := range []string{"forbidden", "unauthorized", "accessdenied", "access denied", "permission_denied", "permission denied", "not authorized", "http 403", "http 401"} {
			if strings.Contains(message, word) {
				status = Denied
				break
			}
		}
	}
	// Diagnostics from commands may contain arbitrary resource data. Keep the
	// public gap stable; external findings alone belong inside the boundary.
	p.fail(status, fmt.Sprintf("%s: %s", label, status))
	return false
}

func (p *probe) run(name string, args ...string) executil.Result {
	if err := p.err(); err != nil {
		return executil.Result{Err: err}
	}
	result := p.service.Runner.Run(p.ctx, name, args...)
	if err := p.err(); err != nil {
		return executil.Result{Err: err}
	}
	if len(result.Stdout) > safety.MaxResponseBytes || len(result.Stderr) > safety.MaxResponseBytes {
		return executil.Result{Err: safety.ErrResponseTooLarge}
	}
	return result
}

func (p *probe) read(label string, dest any, name string, args ...string) bool {
	r := p.run(name, args...)
	if !p.check(label, r) {
		return false
	}
	// Apply the same protection to injected runners as to the real executor.
	data, err := safety.NewRedactor().RedactContext(p.ctx, r.Stdout)
	if err == nil {
		err = p.err()
	}
	if err != nil {
		return p.check(label, executil.Result{Err: err})
	}
	err = safety.DecodeJSON(p.ctx, data, dest)
	if deadlineErr := p.err(); deadlineErr != nil {
		return p.check(label, executil.Result{Err: deadlineErr})
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, safety.ErrResponseTooLarge) || errors.Is(err, safety.ErrResponseComplexity) {
		return p.check(label, executil.Result{Err: err})
	}
	if strings.TrimSpace(data) == "null" || err != nil {
		p.fail(Error, label+": invalid JSON response")
		return false
	}
	return true
}

func (p *probe) cluster() string {
	if !p.online() || !p.require("kubectl") {
		return ""
	}
	p.pin.once.Do(func() {
		p.pin.value = strings.TrimSpace(p.opts.Context)
		if p.pin.value == "" {
			p.pin.result = p.run("kubectl", "config", "current-context")
			p.pin.value = strings.TrimSpace(p.pin.result.Stdout)
		}
		if p.pin.result.Err == nil && (p.pin.value == "" || strings.ContainsAny(p.pin.value, "\r\n")) {
			p.pin.result.Err = errors.New("missing or invalid context")
		}
	})
	if !p.check("resolve Kubernetes context", p.pin.result) {
		return ""
	}
	return p.pin.value
}

func (p *probe) kube(dest any, context string, args ...string) bool {
	return p.read("kubectl "+strings.Join(args, " "), dest, "kubectl", append([]string{"--context", context}, args...)...)
}
