package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/sources"
)

func codeRequest(ids ...string) Request {
	return Request{Plan: fixturePlan(ids...), Mode: ModeCode, Program: "return input", CodeApproved: true}
}

func TestRuntimeContractDoubleConcurrentFailureCacheAndOwnership(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "mcp-http")
	f.fail.Store(true)
	r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		var wg sync.WaitGroup
		results := make([]sources.CallResult, 12)
		errs := make([]error, 12)
		for j := range results {
			wg.Go(func() { results[j], errs[j] = h("k0") })
		}
		wg.Wait()
		for j := range results {
			if errs[j] == nil || !reflect.DeepEqual(results[0], results[j]) {
				t.Errorf("failure replay %d: %+v %v", j, results[j], errs[j])
			}
		}
		results[0].Data[0] = '!'
		results[0].Gaps[0].Reason = "mutated"
		again, _ := h("k0")
		if !json.Valid(again.Data) || again.Gaps[0].Reason == "mutated" {
			t.Error("cache not independently owned")
		}
		return Execution{Values: []Value{{Key: "k0", SourceID: "s", Data: json.RawMessage(`{"fabricated":true}`)}}}, nil
	}}
	result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, codeRequest("s"))
	if !errors.Is(err, ErrIncompleteCollection) || f.calls.Load() != 1 || len(result.Activity) != 1 || len(result.Gaps) == 0 || len(result.MissingRequired) != 1 || result.Evidence[0].Origin != "program_only" {
		t.Fatalf("cache/coverage: %+v %v calls=%d", result, err, f.calls.Load())
	}
}

func TestRuntimeContractDoubleRegistryChangeBeforeHelper(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		f.source.AccessRef = "changed-reader-policy"
		approveFixture(t, root, f.source)
		_, err := h("k0")
		if err == nil {
			t.Error("changed approval accepted")
		}
		return Execution{}, nil
	}}
	result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, codeRequest("s"))
	if !errors.Is(err, ErrIncompleteCollection) || f.contacts.Load() != 0 || len(result.Activity) != 1 || result.Activity[0].Gaps[0].Reason != "approved source changed" {
		t.Fatalf("approval binding: %+v %v contacts=%d", result, err, f.contacts.Load())
	}
}

func TestRuntimeContractDoubleJoinAndHostCoverage(t *testing.T) {
	for _, behavior := range []string{"valid", "omitted", "direct", "fabricated", "failure", "run failure"} {
		t.Run(behavior, func(t *testing.T) {
			root := t.TempDir()
			a := newProvider(t, root, "a", "utcp-http")
			b := newProvider(t, root, "b", "mcp-http")
			if behavior == "failure" {
				a.fail.Store(true)
			}
			r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
				encoded, _ := json.Marshal(i)
				if strings.Contains(string(encoded), fixtureSecret) || strings.Contains(string(encoded), "arguments") {
					t.Error("invocation exposed host data")
				}
				if i.Phase == "join" {
					if h != nil || i.SourceID != "" || i.HostPort != "" || len(i.Keys) != 0 {
						t.Error("join has capabilities")
					}
					if behavior == "omitted" {
						return Execution{Values: i.Values[:1]}, nil
					}
					if behavior == "fabricated" {
						return Execution{Values: []Value{{Key: "unknown", SourceID: "a", Data: json.RawMessage(`true`)}}}, nil
					}
					return Execution{Values: i.Values}, nil
				}
				if behavior == "direct" {
					return Execution{Values: []Value{{Key: i.Keys[0].Key, SourceID: i.SourceID, Data: json.RawMessage(`"unverified"`)}}}, nil
				}
				ex, err := helperValues(ctx, i, h)
				if behavior == "run failure" && i.SourceID == "b" {
					return Execution{}, errors.New("private runtime diagnostic")
				}
				return ex, err
			}}
			result, err := (Service{Sources: a.service, Runtime: r}).Collect(context.Background(), root, codeRequest("a", "b"))
			if behavior == "valid" {
				if err != nil || !result.RequiredKeysSatisfied || len(result.Evidence) != 2 || r.runs != 3 {
					t.Fatalf("join %+v %v", result, err)
				}
			} else if !errors.Is(err, ErrIncompleteCollection) || len(result.Gaps) == 0 {
				t.Fatalf("incomplete %+v %v", result, err)
			}
			if behavior == "direct" {
				if a.calls.Load() != 0 || b.calls.Load() != 0 || len(result.MissingRequired) != 2 || result.Evidence[0].Origin != "program_only" {
					t.Fatalf("direct %+v", result)
				}
			} else if a.calls.Load() != 1 || b.calls.Load() != 1 {
				t.Fatalf("retries: a=%d b=%d", a.calls.Load(), b.calls.Load())
			}
			if behavior == "run failure" && (len(result.Evidence) != 1 || result.Evidence[0].SourceID != "a" || r.runs != 2) {
				t.Fatalf("partial %+v", result)
			}
			if r.opens != 1 || r.closes != 1 {
				t.Fatalf("session lifecycle %+v", r)
			}
		})
	}
}

func TestRuntimeContractDoubleForbiddenHelperAndOutput(t *testing.T) {
	for _, behavior := range []string{"unknown", "wrong source", "duplicate", "mismatch", "invalid JSON"} {
		t.Run(behavior, func(t *testing.T) {
			root := t.TempDir()
			a := newProvider(t, root, "a", "utcp-http")
			b := newProvider(t, root, "b", "utcp-http")
			r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
				switch behavior {
				case "unknown":
					h("missing")
				case "wrong source":
					h("k1")
				case "duplicate":
					return Execution{Values: []Value{{Key: "k0", SourceID: "a", Data: json.RawMessage(`1`)}, {Key: "k0", SourceID: "a", Data: json.RawMessage(`2`)}}}, nil
				case "mismatch":
					return Execution{Values: []Value{{Key: "k0", SourceID: "b", Data: json.RawMessage(`1`)}}}, nil
				case "invalid JSON":
					return Execution{Values: []Value{{Key: "k0", SourceID: "a", Data: json.RawMessage(`{`)}}}, nil
				}
				return Execution{}, nil
			}}
			result, err := (Service{Sources: a.service, Runtime: r}).Collect(context.Background(), root, codeRequest("a", "b"))
			if !errors.Is(err, ErrIncompleteCollection) || a.contacts.Load() != 0 || b.contacts.Load() != 0 || r.runs != 1 || len(result.Gaps) == 0 {
				t.Fatalf("boundary %+v %v runs=%d", result, err, r.runs)
			}
		})
	}
}

func TestRuntimeContractDoubleAuditRedactionAndFailures(t *testing.T) {
	for _, behavior := range []string{"valid", "invalid", "run error", "close error", "oversize"} {
		t.Run(behavior, func(t *testing.T) {
			root := t.TempDir()
			f := newProvider(t, root, "s", "utcp-http")
			r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
				ex, _ := helperValues(ctx, i, h)
				ex.Values[0].Data = json.RawMessage(`{"echo":"` + fixtureSecret + `","token":"remote-secret"}`)
				ex.Audit = []AuditEvent{{Phase: "forged", SourceID: "forged", At: time.Now(), Permission: "net", Value: "https://host/" + fixtureSecret}, {At: time.Now(), Permission: "net", Value: "token=remote-secret"}}
				if behavior == "invalid" {
					ex.Audit = append(ex.Audit, AuditEvent{Permission: "net", Value: "invalid"})
				}
				if behavior == "oversize" {
					for j := 0; j < 600; j++ {
						ex.Audit = append(ex.Audit, AuditEvent{At: time.Now(), Permission: "net", Value: strings.Repeat("x", 2048)})
					}
				}
				if behavior == "run error" {
					return ex, errors.New(fixtureSecret)
				}
				return ex, nil
			}}
			if behavior == "close error" {
				r.close = func(context.Context) error { return errors.New(fixtureSecret) }
			}
			result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, codeRequest("s"))
			if behavior == "valid" {
				if err != nil || !result.RequiredKeysSatisfied {
					t.Fatalf("valid %+v %v", result, err)
				}
			} else if !errors.Is(err, ErrIncompleteCollection) {
				t.Fatalf("audit failure %+v %v", result, err)
			}
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), fixtureSecret) || strings.Contains(string(raw), "remote-secret") || len(result.Audit) < 2 {
				t.Fatalf("audit redaction/preservation %+v %v", result, err)
			}
			for _, e := range result.Audit {
				if e.Phase != "source" || e.SourceID != "s" {
					t.Fatalf("forged attribution %+v", e)
				}
			}
			if behavior == "close error" && !result.RequiredKeysSatisfied {
				t.Fatal("cleanup changed coverage")
			}
		})
	}
}

type observingTransport struct {
	base    http.RoundTripper
	observe func(*http.Request)
}

func (t observingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.observe(r)
	return t.base.RoundTrip(r)
}

func TestRemainingDeadlineBeyondDefault(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	f.service.HTTPClient = &http.Client{Transport: observingTransport{base: http.DefaultTransport, observe: func(r *http.Request) {
		d, ok := r.Context().Deadline()
		if !ok || time.Until(d) < 20*time.Second || time.Until(d) > 30*time.Second {
			t.Errorf("remaining deadline %v %v", d, ok)
		}
	}}}
	for _, mode := range []Mode{ModeOrdinary, ModeCode} {
		r := &runtimeContractDouble{run: helperValues}
		result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, Request{Plan: fixturePlan("s"), Mode: mode, Program: "return input", CodeApproved: true, Timeout: 30 * time.Second})
		if err != nil || !result.RequiredKeysSatisfied {
			t.Fatalf("deadline %+v %v", result, err)
		}
	}
}

func TestRuntimeContractDoubleCancellationAndOpenFailure(t *testing.T) {
	for _, stage := range []string{"probe", "open", "run"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			f := newProvider(t, root, "s", "utcp-http")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &runtimeContractDouble{}
			switch stage {
			case "probe":
				r.probe = func(context.Context) error { cancel(); return errors.New("private") }
			case "open":
				r.open = func(context.Context) (Session, error) { return nil, errors.New("private") }
			case "run":
				r.run = func(context.Context, Invocation, Helper) (Execution, error) {
					cancel()
					return Execution{}, context.Canceled
				}
			}
			result, err := (Service{Sources: f.service, Runtime: r}).Collect(ctx, root, codeRequest("s"))
			if err == nil || f.contacts.Load() != 0 || strings.Contains(err.Error(), "private") {
				t.Fatalf("failure %+v %v", result, err)
			}
			if stage != "open" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation %v", err)
			}
			if stage == "probe" && result.SelectedMode != ModeNone {
				t.Fatal("probe canceled selected execution")
			}
			if stage == "open" && result.SelectedMode != ModeCode {
				t.Fatal("open fallback")
			}
			if stage == "run" && r.closes != 1 {
				t.Fatal("missing cleanup")
			}
		})
	}
}

func TestRuntimeContractDoubleImmutableRequestAndCredentials(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	req := codeRequest("s")
	lookups := 0
	f.service.LookupEnv = func(string) (string, bool) {
		lookups++
		if lookups > 1 {
			return "rotated", true
		}
		return fixtureSecret, true
	}
	r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		req.Plan.Calls[0].Tool = "write"
		req.Plan.Calls[0].Arguments[0] = '!'
		req.Plan.Required[0] = "changed"
		return helperValues(ctx, i, h)
	}}
	before, _ := os.ReadFile(filepath.Join(root, sources.RegistryFilename))
	result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, req)
	after, _ := os.ReadFile(filepath.Join(root, sources.RegistryFilename))
	if err != nil || f.calls.Load() != 1 || lookups != 1 || !result.RequiredKeysSatisfied || string(before) != string(after) {
		t.Fatalf("immutable %+v %v lookups=%d", result, err, lookups)
	}
}

func TestRuntimeContractDoubleJoinOnlyCompletedEmissions(t *testing.T) {
	root := t.TempDir()
	a := newProvider(t, root, "a", "utcp-http")
	newProvider(t, root, "b", "utcp-http")
	r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		if i.Phase == "join" {
			return Execution{Values: []Value{{Key: "k0", SourceID: "a", Data: json.RawMessage(`true`)}}}, nil
		}
		h(i.Keys[0].Key)
		return Execution{}, nil
	}}
	result, err := (Service{Sources: a.service, Runtime: r}).Collect(context.Background(), root, codeRequest("a", "b"))
	if !errors.Is(err, ErrIncompleteCollection) || len(result.Evidence) != 0 || len(result.MissingRequired) != 2 {
		t.Fatalf("invented join key %+v %v", result, err)
	}
}

func TestRuntimeContractDoubleFixedSelectionReasons(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	for _, credential := range []string{"", "bad\nvalue", "bad\rvalue", strings.Repeat("x", 65537)} {
		f.service.LookupEnv = func(string) (string, bool) { return credential, true }
		r := &runtimeContractDouble{}
		result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, codeRequest("s"))
		if !errors.Is(err, ErrCodeUnavailable) || r.probes != 0 || result.Reason != "code credential unavailable" {
			t.Fatalf("credential selection %+v %v", result, err)
		}
	}
}

func TestRuntimeContractDoubleSharedSessionDeadline(t *testing.T) {
	root := t.TempDir()
	a := newProvider(t, root, "a", "utcp-http")
	newProvider(t, root, "b", "utcp-http")
	var deadline time.Time
	r := &runtimeContractDouble{probe: func(ctx context.Context) error { deadline, _ = ctx.Deadline(); return nil }, run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		d, _ := ctx.Deadline()
		if !d.Equal(deadline) {
			t.Error("fresh phase deadline")
		}
		if i.Phase == "join" {
			return Execution{Values: i.Values}, nil
		}
		return helperValues(ctx, i, h)
	}, close: func(ctx context.Context) error {
		d, _ := ctx.Deadline()
		if time.Until(d) > 2*time.Second || time.Until(d) < time.Second {
			t.Error("cleanup grace")
		}
		return nil
	}}
	result, err := (Service{Sources: a.service, Runtime: r}).Collect(context.Background(), root, codeRequest("a", "b"))
	if err != nil || r.opens != 1 || r.runs != 3 || r.closes != 1 {
		t.Fatalf("session %+v %v %+v", result, err, r)
	}
}

func ExampleResult_coverage() {
	fmt.Println("required_keys_satisfied reports successful host calls with retained values, not factual truth")
	// Output: required_keys_satisfied reports successful host calls with retained values, not factual truth
}

func TestRuntimeContractDoubleContextErrorsRemainDetectable(t *testing.T) {
	for _, stage := range []string{"probe", "open", "run", "close"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			f := newProvider(t, root, "s", "utcp-http")
			r := &runtimeContractDouble{run: helperValues}
			switch stage {
			case "probe":
				r.probe = func(context.Context) error { return context.DeadlineExceeded }
			case "open":
				r.open = func(context.Context) (Session, error) { return nil, context.DeadlineExceeded }
			case "run":
				r.run = func(context.Context, Invocation, Helper) (Execution, error) {
					return Execution{}, context.DeadlineExceeded
				}
			case "close":
				r.close = func(context.Context) error { return context.DeadlineExceeded }
			}
			result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, codeRequest("s"))
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrIncompleteCollection) {
				t.Fatalf("lost context sentinel: %+v %v", result, err)
			}
		})
	}
}

func TestRuntimeContractDoubleCredentialPreflightCancellation(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.service.LookupEnv = func(string) (string, bool) { cancel(); return "", false }
	r := &runtimeContractDouble{}
	result, err := (Service{Sources: f.service, Runtime: r}).Collect(ctx, root, codeRequest("s"))
	if !errors.Is(err, context.Canceled) || result.SelectedMode != ModeNone || f.contacts.Load() != 0 || r.probes != 0 {
		t.Fatalf("preflight cancellation %+v %v", result, err)
	}
}
