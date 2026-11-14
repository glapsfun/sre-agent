package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/sources"
)

func TestRuntimeContractDoubleConcurrencyLimitAndSealedHelper(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	var active, peak atomic.Int32
	f.service.HTTPClient = &http.Client{Transport: observingTransport{base: http.DefaultTransport, observe: func(r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond * 5)
	}}}
	var helper Helper
	r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		helper = h
		var wg sync.WaitGroup
		values := make([]Value, len(i.Keys))
		for j, k := range i.Keys {
			wg.Go(func() {
				result, err := h(k.Key)
				if err != nil {
					t.Error(err)
				}
				values[j] = Value{Key: k.Key, SourceID: k.SourceID, Data: result.Data}
			})
		}
		wg.Wait()
		return Execution{Values: values}, nil
	}}
	result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, codeRequest("s", "s", "s", "s", "s", "s"))
	if err != nil || peak.Load() != 2 || f.calls.Load() != 6 {
		t.Fatalf("concurrency peak=%d calls=%d result=%+v err=%v", peak.Load(), f.calls.Load(), result, err)
	}
	before, _ := json.Marshal(result)
	contacts := f.contacts.Load()
	if _, err := helper("k0"); err == nil {
		t.Fatal("sealed helper accepted")
	}
	after, _ := json.Marshal(result)
	if string(before) != string(after) || f.contacts.Load() != contacts {
		t.Fatal("sealed helper changed collection")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRuntimeContractDoublePendingCleanupLateCompletionImmutable(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	started := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	var once sync.Once
	f.service.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-release
		return nil, errors.New("late transport error with " + fixtureSecret)
	})}
	var helper Helper
	var closeRemaining time.Duration
	r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		helper = h
		go func() { defer close(returned); h("k0") }()
		<-started
		return Execution{}, nil
	}, close: func(ctx context.Context) error {
		d, _ := ctx.Deadline()
		closeRemaining = time.Until(d)
		return ctx.Err()
	}}
	start := time.Now()
	result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, codeRequest("s"))
	if !errors.Is(err, ErrIncompleteCollection) || time.Since(start) > 3*time.Second || closeRemaining > 100*time.Millisecond || r.closes != 1 || len(result.Activity) != 1 {
		t.Fatalf("shared cleanup %+v %v grace=%v duration=%v", result, err, closeRemaining, time.Since(start))
	}
	before, _ := json.Marshal(result)
	close(release)
	<-returned
	if _, err := helper("k0"); err == nil {
		t.Fatal("late helper accepted")
	}
	after, _ := json.Marshal(result)
	if string(before) != string(after) {
		t.Fatal("late callback mutated returned state")
	}
}

func TestRuntimeContractDoubleCacheOverflowTerminal(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	// Keep 950,000 UTF-8 bytes per response while limiting regex rune work under race instrumentation.
	f.payload.Store(`{"data":"` + strings.Repeat("\U00020000", 237500) + `"}`)
	ids := make([]string, 20)
	for i := range ids {
		ids[i] = "s"
	}
	r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		var failed atomic.Bool
		for offset := 0; offset < len(i.Keys) && !failed.Load(); offset += 2 {
			var wg sync.WaitGroup
			for _, key := range i.Keys[offset : offset+2] {
				wg.Go(func() {
					result, err := h(key.Key)
					if err == nil {
						return
					}
					failed.Store(true)
					if len(result.Data) != 0 || len(result.Gaps) == 0 {
						t.Error("overflow retained raw data")
					}
					again, e := h(key.Key)
					if e == nil || !reflect.DeepEqual(result, again) {
						t.Errorf("overflow not cached %+v %+v %v", result, again, e)
					}
				})
			}
			wg.Wait()
		}
		if !failed.Load() {
			t.Error("cache overflow not detected")
		}
		return Execution{}, nil
	}}
	req := codeRequest(ids...)
	req.Timeout = 60 * time.Second
	result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, req)
	if !errors.Is(err, ErrIncompleteCollection) || f.calls.Load() != 18 || len(result.Activity) != 18 || !hasGap(result, "helper cache budget exceeded") {
		t.Fatalf("cache overflow calls=%d activity=%d gaps=%+v err=%v", f.calls.Load(), len(result.Activity), result.Gaps, err)
	}
}
func hasGap(r Result, reason string) bool {
	for _, g := range r.Gaps {
		if g.Reason == reason {
			return true
		}
	}
	return false
}

func TestRuntimeContractDoubleOutputBudgets(t *testing.T) {
	for _, behavior := range []string{"aggregate", "join replacement", "join wrapper", "post-redaction"} {
		t.Run(behavior, func(t *testing.T) {
			root := t.TempDir()
			a := newProvider(t, root, "a", "utcp-http")
			newProvider(t, root, "b", "utcp-http")
			r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
				if i.Phase == "join" {
					if behavior == "join wrapper" {
						t.Error("oversized join input ran")
					}
					return Execution{Values: []Value{{Key: "k0", SourceID: "a", Data: json.RawMessage(`"` + strings.Repeat("j", 60000) + `"`)}, {Key: "k1", SourceID: "b", Data: json.RawMessage(`true`)}}}, nil
				}
				h(i.Keys[0].Key)
				size := 32000
				switch behavior {
				case "aggregate":
					size = 40000
				case "post-redaction":
					return Execution{Values: []Value{{Key: i.Keys[0].Key, SourceID: i.SourceID, Data: json.RawMessage(`{"token":"` + strings.Repeat("x", 100000) + `"}`)}}}, nil
				case "join wrapper":
					size = 32720
				}
				return Execution{Values: []Value{{Key: i.Keys[0].Key, SourceID: i.SourceID, Data: json.RawMessage(`"` + strings.Repeat("x", size) + `"`)}}}, nil
			}}
			result, err := (Service{Sources: a.service, Runtime: r}).Collect(context.Background(), root, codeRequest("a", "b"))
			switch behavior {
			case "join replacement", "post-redaction":
				if err != nil || !result.RequiredKeysSatisfied || len(result.Evidence) != 2 {
					t.Fatalf("fresh/redacted budget %+v %v", result, err)
				}
			case "aggregate":
				if !errors.Is(err, ErrIncompleteCollection) || len(result.Evidence) != 1 || r.runs != 2 {
					t.Fatalf("aggregate count=%d runs=%d gaps=%+v err=%v", len(result.Evidence), r.runs, result.Gaps, err)
				}
			case "join wrapper":
				if !errors.Is(err, ErrIncompleteCollection) || !hasGap(result, "join input budget exceeded") || r.runs != 2 {
					t.Fatalf("wrapper runs=%d gaps=%+v err=%v", r.runs, result.Gaps, err)
				}
			}
		})
	}
}

func TestOrdinaryBudgetRetainsPlanOrderAndReceipt(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	f.payload.Store(`{"data":"` + strings.Repeat("x", 40000) + `"}`)
	result, err := (Service{Sources: f.service}).Collect(context.Background(), root, Request{Plan: fixturePlan("s", "s"), Mode: ModeOrdinary})
	if !errors.Is(err, ErrIncompleteCollection) || len(result.Evidence) != 1 || result.Evidence[0].Key != "k0" || len(result.Activity) != 2 || !result.Activity[1].Success || !reflect.DeepEqual(result.MissingRequired, []string{"k1"}) {
		t.Fatalf("ordered budget evidence=%d activity=%+v missing=%v err=%v", len(result.Evidence), result.Activity, result.MissingRequired, err)
	}
}

func TestRuntimeContractDoubleAuditValidationBounds(t *testing.T) {
	for _, event := range []AuditEvent{
		{At: time.Now(), Permission: ""}, {At: time.Now(), Permission: strings.Repeat("p", 65)},
		{At: time.Now(), Permission: "net", Value: strings.Repeat("v", 2049)},
		{At: time.Now(), Permission: "net", Value: "bad\ncontrol"},
		{At: time.Now(), Permission: "net", Value: string([]byte{0xff})},
		{At: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), Permission: "net"},
	} {
		t.Run(fmt.Sprintf("%d-%d-%d", event.At.Year(), len(event.Permission), len(event.Value)), func(t *testing.T) {
			root := t.TempDir()
			f := newProvider(t, root, "s", "utcp-http")
			r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
				ex, _ := helperValues(ctx, i, h)
				ex.Audit = []AuditEvent{event}
				return ex, nil
			}}
			result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, codeRequest("s"))
			if !errors.Is(err, ErrIncompleteCollection) || !hasGap(result, "invalid code permission audit") || len(result.Audit) != 0 {
				t.Fatalf("audit validation %+v %v", result, err)
			}
		})
	}
}

func TestRuntimeContractDoubleCancellationReplaysTerminal(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	started := make(chan struct{})
	var once sync.Once
	f.service.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		result, err := h("k0")
		again, e := h("k0")
		if err == nil || e == nil || !reflect.DeepEqual(result, again) {
			t.Errorf("canceled cache differs %+v %+v %v %v", result, again, err, e)
		}
		return Execution{}, nil
	}}
	req := codeRequest("s")
	req.Timeout = 50 * time.Millisecond
	result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, req)
	if !errors.Is(err, context.DeadlineExceeded) || len(result.Activity) != 1 {
		t.Fatalf("cancellation %+v %v", result, err)
	}
}

func TestOrdinaryNoEarlyCredentialRead(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	lookups := 0
	f.service.LookupEnv = func(string) (string, bool) { lookups++; return fixtureSecret, true }
	result, err := (Service{Sources: f.service}).Collect(context.Background(), root, Request{Plan: fixturePlan("s"), Mode: ModeOrdinary})
	if err != nil || lookups != 1 {
		t.Fatalf("credential reads %d %+v %v", lookups, result, err)
	}
}

func TestRuntimeContractDoubleEscapedScalarRedaction(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		h("k0")
		return Execution{Values: []Value{{Key: "k0", SourceID: "s", Data: json.RawMessage(`"fixture-private-\u0076alue"`)}}}, nil
	}}
	result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, codeRequest("s"))
	if err != nil || string(result.Evidence[0].Data) != `"[REDACTED]"` {
		t.Fatalf("scalar %+v %v", result, err)
	}
}

func TestProviderFixtureRejectsWrites(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	req, _ := http.NewRequest("POST", strings.TrimSuffix(f.source.Endpoint, "manual")+"read", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+fixtureSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusForbidden || f.calls.Load() != 0 {
		t.Fatalf("write accepted status=%d calls=%d", resp.StatusCode, f.calls.Load())
	}
}

func TestRuntimeContractDoubleIntermediateArrayOverheadsShareBudget(t *testing.T) {
	root := t.TempDir()
	a := newProvider(t, root, "a", "utcp-http")
	newProvider(t, root, "b", "utcp-http")
	r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		if i.Phase != "source" {
			t.Error("oversized intermediate arrays reached join")
			return Execution{}, nil
		}
		h(i.Keys[0].Key)
		n := 32728
		if i.SourceID == "b" {
			n = 32729
		}
		// Each compact one-value array has 40 bytes of key/source/data framing.
		return Execution{Values: []Value{{Key: i.Keys[0].Key, SourceID: i.SourceID, Data: json.RawMessage(`"` + strings.Repeat("x", n) + `"`)}}}, nil
	}}
	result, err := (Service{Sources: a.service, Runtime: r}).Collect(context.Background(), root, codeRequest("a", "b"))
	if !errors.Is(err, ErrIncompleteCollection) || len(result.Evidence) != 1 || !hasGap(result, "invalid or oversized code output") {
		t.Fatalf("aggregate arrays evidence=%d gaps=%+v err=%v", len(result.Evidence), result.Gaps, err)
	}
}

func TestRuntimeContractDoubleConcurrentCancellationReplaysSameOutcome(t *testing.T) {
	root := t.TempDir()
	f := newProvider(t, root, "s", "utcp-http")
	f.service.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		time.Sleep(10 * time.Millisecond)
		return nil, r.Context().Err()
	})}
	r := &runtimeContractDouble{run: func(ctx context.Context, i Invocation, h Helper) (Execution, error) {
		var wg sync.WaitGroup
		results := make([]sources.CallResult, 8)
		errs := make([]error, 8)
		for j := range results {
			wg.Go(func() { results[j], errs[j] = h("k0") })
		}
		wg.Wait()
		for j := range results {
			if errs[j] == nil || !reflect.DeepEqual(results[0], results[j]) {
				t.Errorf("concurrent cancellation differs: first=%+v next=%+v error=%v", results[0], results[j], errs[j])
			}
		}
		return Execution{}, nil
	}}
	req := codeRequest("s")
	req.Timeout = 50 * time.Millisecond
	result, err := (Service{Sources: f.service, Runtime: r}).Collect(context.Background(), root, req)
	if !errors.Is(err, context.DeadlineExceeded) || len(result.Activity) != 1 {
		t.Fatalf("cancellation %+v %v", result, err)
	}
}
