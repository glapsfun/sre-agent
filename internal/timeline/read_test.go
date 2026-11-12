package timeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/glapsfun/sre-agent/internal/executil"
	"github.com/glapsfun/sre-agent/internal/safety"
)

type streamKube struct {
	stream func(context.Context, io.Writer, ...string) executil.Result
}

func (k streamKube) Context() string { return "pinned" }
func (k streamKube) Run(context.Context, ...string) executil.Result {
	panic("list reads must stream")
}
func (k streamKube) Stream(ctx context.Context, w io.Writer, args ...string) executil.Result {
	return k.stream(ctx, w, args...)
}

func listKube(body string) streamKube {
	return streamKube{stream: func(_ context.Context, w io.Writer, _ ...string) executil.Result {
		_, err := io.WriteString(w, body)
		return executil.Result{Err: err}
	}}
}

func TestReadListStreamsEveryMember(t *testing.T) {
	const count = 70000
	member := `{"kind":"Pod","metadata":{"name":"api","namespace":"ns","uid":"id"},"padding":"` + strings.Repeat("x", 80) + `"}`
	kube := streamKube{stream: func(_ context.Context, w io.Writer, args ...string) executil.Result {
		if strings.Join(args, " ") != "get pods --namespace=ns --output=json" {
			return executil.Result{Err: errors.New("wrong list request")}
		}
		if _, err := io.WriteString(w, `{"apiVersion":"v1","kind":"List","items":[`); err != nil {
			return executil.Result{Err: err}
		}
		for i := 0; i < count; i++ {
			if i > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return executil.Result{Err: err}
				}
			}
			if _, err := io.WriteString(w, member); err != nil {
				return executil.Result{Err: err}
			}
		}
		_, err := io.WriteString(w, `]}`)
		return executil.Result{Err: err}
	}}
	seen := 0
	err := ReadList(context.Background(), kube, "pods", []string{"--namespace=ns"}, func(raw json.RawMessage) error {
		if string(raw) != member {
			return errors.New("changed member")
		}
		seen++
		return nil
	})
	if err != nil || seen != count {
		t.Fatalf("seen=%d error=%v", seen, err)
	}
}

func TestReadListRejectsMalformedEnvelope(t *testing.T) {
	for _, body := range []string{
		`{"apiVersion":"v1","kind":"List","items":[{}]} {}`,
		`{"apiVersion":"v1","kind":"List","items":[{}]} garbage`,
		`{"apiVersion":"v1","kind":"List","items":[{}`,
		`{"apiVersion":"v1","kind":"List","items":null}`,
		`{"apiVersion":"v1","kind":"List"}`,
		`{"apiVersion":"v1","kind":"Pod","items":[]}`,
		`{"apiVersion":"v1","kind":"List","items":[],"items":[]}`,
		`{"apiVersion":"v1","kind":"List","items":[{},]}`,
		`{"apiVersion":"v1","kind":"List","items":[],}`,
		`{"apiVersion":"v1","kind":"List","metadata":{"continue":"next"},"items":[]}`,
	} {
		t.Run(body, func(t *testing.T) {
			err := ReadList(context.Background(), listKube(body), "pods", nil, func(json.RawMessage) error { return nil })
			if err == nil {
				t.Fatal("accepted invalid or incomplete list")
			}
		})
	}
}

func TestReadListRejectsNullKeyAndJoinsProducer(t *testing.T) {
	stopped := make(chan struct{})
	kube := streamKube{stream: func(_ context.Context, w io.Writer, _ ...string) executil.Result {
		defer close(stopped)
		_, err := io.WriteString(w, `{"apiVersion":"v1","kind":"List",null :0,"items":[{}]}`)
		return executil.Result{Err: err}
	}}
	seen := 0
	err := ReadList(context.Background(), kube, "pods", nil, func(json.RawMessage) error { seen++; return nil })
	select {
	case <-stopped:
	default:
		t.Fatal("producer was not joined")
	}
	if err == nil || seen != 0 {
		t.Fatalf("invalid key accepted: visited=%d error=%v", seen, err)
	}
}

func TestReadListProducerDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, message, diagnostic string }{
		{"denied", "Error from server (Forbidden): pods is forbidden; detail=private-marker password=hidden-password", "Forbidden"},
		{"missing", "error: the server doesn't have a resource type widgets; detail=private-marker password=hidden-password", "doesn't have a resource type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result executil.Result
			kube := streamKube{stream: func(ctx context.Context, w io.Writer, _ ...string) executil.Result {
				result = (executil.Runner{Secrets: []string{"private-marker"}}).Stream(ctx, w, "sh", "-c", `printf '{"apiVersion":"v1","kind":"List","items":[],"raw":"stdout-only-marker"}'; printf '%s\n' "$1" >&2; exit 1`, "test", tc.message)
				return result
			}}
			err := ReadList(context.Background(), kube, "pods", nil, func(json.RawMessage) error { return nil })
			var producer *ProducerError
			if !errors.As(err, &producer) {
				t.Fatalf("producer details unavailable: %v", err)
			}
			if producer.ExitCode == nil || *producer.ExitCode != 1 || !strings.Contains(producer.Stderr, tc.diagnostic) {
				t.Fatal("producer exit status or diagnostic was lost")
			}
			if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
				t.Fatalf("diagnostic unavailable: %v", err)
			}
			if !errors.Is(err, result.Err) {
				t.Fatal("producer cause cannot be unwrapped")
			}
			for _, secret := range []string{"private-marker", "hidden-password", "stdout-only-marker"} {
				if strings.Contains(err.Error(), secret) || strings.Contains(producer.Stderr, secret) {
					t.Fatal("producer error exposed a secret or raw stdout")
				}
			}
		})
	}
}

func TestReadListPerRecordBound(t *testing.T) {
	body := `{"apiVersion":"v1","kind":"List","items":[{"padding":"` + strings.Repeat("x", safety.MaxResponseBytes) + `"}]}`
	err := ReadList(context.Background(), listKube(body), "pods", nil, func(json.RawMessage) error { t.Fatal("visited oversized member"); return nil })
	if !errors.Is(err, safety.ErrResponseTooLarge) {
		t.Fatalf("error=%v", err)
	}
}

func TestReadListProducerFailure(t *testing.T) {
	want := errors.New("producer failed")
	kube := streamKube{stream: func(_ context.Context, w io.Writer, _ ...string) executil.Result {
		_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"List","items":[{}]}`)
		return executil.Result{Err: want}
	}}
	err := ReadList(context.Background(), kube, "pods", nil, func(json.RawMessage) error { return nil })
	if !errors.Is(err, want) {
		t.Fatalf("error=%v", err)
	}
}

func TestReadListCancellationAndConsumerRejection(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "consumer"}[reject], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stopped := make(chan struct{})
			kube := streamKube{stream: func(ctx context.Context, w io.Writer, _ ...string) executil.Result {
				defer close(stopped)
				_, err := io.WriteString(w, `{"apiVersion":"v1","kind":"List","items":[{}`)
				if err != nil {
					return executil.Result{Err: err}
				}
				<-ctx.Done()
				return executil.Result{Err: ctx.Err()}
			}}
			want := errors.New("consumer rejected")
			done := make(chan error, 1)
			go func() {
				done <- ReadList(ctx, kube, "pods", nil, func(json.RawMessage) error {
					if reject {
						return want
					}
					cancel()
					return nil
				})
			}()
			select {
			case err := <-done:
				if reject && !errors.Is(err, want) || !reject && !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
				select {
				case <-stopped:
				default:
					t.Fatal("producer not joined")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("reader or producer did not terminate")
			}
		})
	}
}

func TestReadListEnvelopeBudgetAndComplexMember(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"envelope", `{"apiVersion":"v1","kind":"List","items":[],"first":"` + strings.Repeat("x", safety.MaxResponseBytes/2) + `","second":"` + strings.Repeat("x", safety.MaxResponseBytes/2) + `"}`, safety.ErrResponseTooLarge},
		{"complex member", `{"apiVersion":"v1","kind":"List","items":[{"nested":[` + strings.Repeat(`{},`, safety.MaxJSONStructure/2) + `{}]}]}`, safety.ErrResponseComplexity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ReadList(context.Background(), listKube(tc.body), "pods", nil, func(json.RawMessage) error { return nil })
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestReadListRealProducerStopsAfterRejection(t *testing.T) {
	kube := streamKube{stream: func(ctx context.Context, w io.Writer, _ ...string) executil.Result {
		return (executil.Runner{Timeout: 3 * time.Second}).Stream(ctx, w, "sh", "-c", `printf '{"apiVersion":"v1","kind":"List","items":[{}'; sleep 10`)
	}}
	want := errors.New("consumer rejected")
	start := time.Now()
	err := ReadList(context.Background(), kube, "pods", nil, func(json.RawMessage) error { return want })
	var producer *ProducerError
	if errors.As(err, &producer) {
		t.Fatal("producer cleanup masked consumer rejection")
	}
	if !errors.Is(err, want) || time.Since(start) > 2*time.Second {
		t.Fatalf("error=%v duration=%s", err, time.Since(start))
	}
}

func TestReadListCancellationOfIdleProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	stopped := make(chan struct{})
	kube := streamKube{stream: func(ctx context.Context, w io.Writer, _ ...string) executil.Result {
		close(started)
		defer close(stopped)
		<-ctx.Done()
		return executil.Result{Err: ctx.Err()}
	}}
	done := make(chan error, 1)
	go func() { done <- ReadList(ctx, kube, "pods", nil, func(json.RawMessage) error { return nil }) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
		select {
		case <-stopped:
		default:
			t.Fatal("producer not joined")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle producer did not stop")
	}
}
