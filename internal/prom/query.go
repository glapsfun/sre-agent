package prom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/discover"
	"github.com/glapsfun/sre-agent/internal/executil"
	"github.com/glapsfun/sre-agent/internal/safety"
)

type Options struct {
	URL     string
	Query   string
	Context string
	Range   time.Duration
	Step    time.Duration
	Top     int
	Timeout time.Duration
}

type Series struct {
	Labels map[string]string `json:"labels"`
	Summary
}

type Report struct {
	Begin         string    `json:"begin,omitempty"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	StepSeconds   float64   `json:"step_seconds"`
	Series        []Series  `json:"series"`
	OmittedSeries int       `json:"omitted_series"`
	Infos         []string  `json:"infos"`
	Finish        string    `json:"end_external,omitempty"`
}

// Service allows other adapters to use the same range query and summaries.
// Injected clients and discovery functions must honor the supplied context.
type Service struct {
	Client   *http.Client
	Now      func() time.Time
	Discover func(context.Context, discover.Options) (discover.Report, error)
}

func Validate(opts Options) error {
	if strings.TrimSpace(opts.Query) == "" {
		return errors.New("PromQL query is required")
	}
	if opts.Range < 0 || opts.Step < 0 || opts.Timeout < 0 || opts.Top < 0 {
		return errors.New("range, step, timeout and top must be positive")
	}
	if opts.Range%time.Millisecond != 0 {
		return errors.New("range must use whole milliseconds")
	}
	if opts.Step%time.Millisecond != 0 {
		return errors.New("step must use whole milliseconds")
	}
	if strings.ContainsAny(opts.Context, "\r\n") {
		return errors.New("invalid Kubernetes context")
	}
	if opts.URL != "" {
		u, err := url.Parse(opts.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Fragment != "" || u.Opaque != "" {
			return errors.New("--url must be an HTTP(S) base URL without a fragment")
		}
	}
	return nil
}

func Query(ctx context.Context, opts Options) (Report, error) { return (Service{}).Query(ctx, opts) }

func (s Service) Query(ctx context.Context, opts Options) (Report, error) {
	if err := Validate(opts); err != nil {
		return Report{}, err
	}
	if opts.Range == 0 {
		opts.Range = time.Hour
	}
	if opts.Step == 0 {
		opts.Step = 30 * time.Second
	}
	if opts.Top == 0 {
		opts.Top = 20
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if s.Now == nil {
		s.Now = time.Now
	}
	end := s.Now().UTC().Truncate(time.Millisecond)
	start := end.Add(-opts.Range)
	// Decimal seconds can round a supported 1001ms interval down to 1000ms.
	params := url.Values{"query": {opts.Query}, "start": {timestamp(start)}, "end": {timestamp(end)}, "step": {strconv.FormatInt(opts.Step.Milliseconds(), 10) + "ms"}, "timeout": {strconv.FormatFloat(opts.Timeout.Seconds(), 'f', -1, 64)}}
	var body []byte
	var err error
	redactor := safety.NewRedactor()
	if opts.URL != "" {
		u, _ := url.Parse(opts.URL)
		secrets := []string{}
		if u.User != nil {
			password, _ := u.User.Password()
			secrets = append(secrets, u.User.Username(), password)
		}
		for _, values := range u.Query() {
			secrets = append(secrets, values...)
		}
		redactor = safety.NewRedactor(secrets...)
		u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/query_range"
		if u.RawPath != "" {
			u.RawPath = strings.TrimRight(u.RawPath, "/") + "/api/v1/query_range"
		}
		query := u.Query()
		for name, values := range params {
			query[name] = values
		}
		u.RawQuery = query.Encode()
		body, err = s.httpRead(ctx, u.String())
	} else {
		body, err = s.proxyRead(ctx, opts, params)
	}
	if err != nil {
		return Report{}, err
	}
	return decode(ctx, body, start, end, opts.Step, opts.Top, redactor)
}

func timestamp(t time.Time) string {
	// RFC3339 supports arbitrary time.Time values, but decimal Unix seconds
	// preserve the exact fractional evaluation grid without float conversion.
	seconds := t.Unix()
	if t.Nanosecond() == 0 {
		return strconv.FormatInt(seconds, 10)
	}
	if seconds < 0 {
		return t.Format(time.RFC3339Nano)
	}
	return strconv.FormatInt(seconds, 10) + "." + strings.TrimRight(fmt.Sprintf("%09d", t.Nanosecond()), "0")
}

func (s Service) httpRead(ctx context.Context, endpoint string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("invalid Prometheus request URL")
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Transport diagnostics can echo credentials and arbitrary URL segments.
		return nil, errors.New("Prometheus HTTP request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, safety.MaxResponseBytes+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(body) > safety.MaxResponseBytes {
		return nil, safety.ErrResponseTooLarge
	}
	if err != nil {
		return nil, errors.New("read Prometheus HTTP response failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Preserve documented API errors, but never present an arbitrary proxy
		// HTML page or transport body as trusted diagnostics.
		var status struct {
			Status string `json:"status"`
		}
		if safety.DecodeJSON(ctx, string(body), &status) != nil || status.Status != "error" {
			return nil, fmt.Errorf("Prometheus HTTP %d", response.StatusCode)
		}
	}
	return body, nil
}

func (s Service) proxyRead(ctx context.Context, opts Options, params url.Values) ([]byte, error) {
	if os.Getenv("SRE_SKIP_CLUSTER") == "true" {
		return nil, errors.New("cluster discovery skipped; provide --url")
	}
	runner := executil.Runner{Timeout: opts.Timeout}
	kube, err := executil.NewKubernetes(ctx, runner, opts.Context)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("cannot resolve Kubernetes context; provide --url")
	}
	discovery := s.Discover
	if discovery == nil {
		discovery = discover.Discover
	}
	report, err := discovery(ctx, discover.Options{Context: kube.Context(), Only: []string{"obs"}, Timeout: opts.Timeout})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, errors.New("observability discovery failed; provide --url")
	}
	obs := report.Probes["obs"].Findings.Obs
	if obs == nil || !obs.EndpointInventoryComplete {
		return nil, errors.New("Prometheus endpoint inventory is incomplete or unavailable; provide --url")
	}
	var matches []discover.Endpoint
	for _, ep := range obs.Endpoints {
		if ep.Component == "prometheus" {
			matches = append(matches, ep)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("discovered %d Prometheus services; require exactly one or provide --url", len(matches))
	}
	ep := matches[0]
	if !resourceName.MatchString(ep.Namespace) || !resourceName.MatchString(ep.Service) {
		return nil, errors.New("invalid discovered Prometheus identity; provide --url")
	}
	ports := map[int]bool{}
	for _, port := range ep.Ports {
		if port < 1 || port > 65535 {
			return nil, errors.New("invalid discovered Prometheus port; provide --url")
		}
		ports[port] = true
	}
	port := 9090
	if !ports[9090] {
		if len(ports) != 1 {
			return nil, errors.New("ambiguous or missing Prometheus port; provide --url")
		}
		for p := range ports {
			port = p
		}
	}
	path := "/api/v1/namespaces/" + url.PathEscape(ep.Namespace) + "/services/" + url.PathEscape(ep.Service) + ":" + strconv.Itoa(port) + "/proxy/api/v1/query_range"
	return readProxy(ctx, kube, path, params)
}

var resourceName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

type apiResponse struct {
	Status    string   `json:"status"`
	Error     string   `json:"error"`
	ErrorType string   `json:"errorType"`
	Warnings  []string `json:"warnings"`
	Infos     []string `json:"infos"`
	Data      *apiData `json:"data"`
}

type apiData struct {
	ResultType string      `json:"resultType"`
	Result     []apiSeries `json:"result"`
}

type apiSeries struct {
	Metric     map[string]json.RawMessage `json:"metric"`
	Values     [][]json.RawMessage        `json:"values"`
	Histograms json.RawMessage            `json:"histograms"`
}

func externalError(ctx context.Context, redactor safety.Redactor, prefix, text string) error {
	clean, err := redactor.RedactContext(ctx, text)
	if err != nil {
		return err
	}
	wrapper, err := safety.NewWrapper()
	if err != nil {
		return err
	}
	return errors.New(prefix + ": " + wrapper.Wrap("Prometheus diagnostic", clean))
}

func decode(ctx context.Context, body []byte, start, end time.Time, step time.Duration, top int, redactor safety.Redactor) (Report, error) {
	var api apiResponse
	if err := decodeResponse(ctx, body, &api); err != nil {
		if ctx.Err() != nil {
			return Report{}, ctx.Err()
		}
		if errors.Is(err, safety.ErrResponseTooLarge) || errors.Is(err, safety.ErrResponseComplexity) {
			return Report{}, err
		}
		return Report{}, errors.New("invalid Prometheus response schema")
	}
	if api.Status == "error" {
		return Report{}, externalError(ctx, redactor, "Prometheus API error", api.ErrorType+": "+api.Error)
	}
	if api.Status != "success" || api.Data == nil {
		return Report{}, errors.New("invalid Prometheus response status or data")
	}
	if len(api.Warnings) > 0 {
		return Report{}, externalError(ctx, redactor, "Prometheus warning", strings.Join(api.Warnings, "\n"))
	}
	if api.Data.ResultType != "matrix" {
		return Report{}, errors.New("expected Prometheus matrix response")
	}
	if api.Data.Result == nil {
		return Report{}, errors.New("invalid Prometheus result array")
	}
	report := Report{Start: start, End: end, StepSeconds: step.Seconds(), Series: []Series{}, Infos: []string{}}
	for _, info := range api.Infos {
		clean, err := redactor.RedactContext(ctx, info)
		if err != nil {
			return Report{}, err
		}
		report.Infos = append(report.Infos, clean)
	}
	type ranked struct {
		series   Series
		key      string
		fraction float64
		exponent int
	}
	rows := make([]ranked, 0, len(api.Data.Result))
	for _, row := range api.Data.Result {
		if ctx.Err() != nil {
			return Report{}, ctx.Err()
		}
		if len(row.Histograms) > 0 {
			return Report{}, errors.New("native histogram samples cannot be summarized; use a float conversion query such as histogram_sum(), histogram_count() or histogram_avg()")
		}
		if row.Metric == nil || row.Values == nil {
			return Report{}, errors.New("invalid Prometheus series labels or values")
		}
		labels := map[string]string{}
		for key, value := range row.Metric {
			var label string
			if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &label) != nil {
				return Report{}, errors.New("invalid Prometheus label value")
			}
			labels[key] = label
		}
		encoded, _ := json.Marshal(labels)
		clean, err := redactor.RedactContext(ctx, string(encoded))
		if err != nil {
			return Report{}, err
		}
		labels = map[string]string{}
		if err = json.Unmarshal([]byte(clean), &labels); err != nil {
			return Report{}, errors.New("invalid redacted Prometheus labels")
		}
		samples := make([]Sample, 0, len(row.Values))
		for _, pair := range row.Values {
			if ctx.Err() != nil {
				return Report{}, ctx.Err()
			}
			if len(pair) != 2 {
				return Report{}, errors.New("invalid Prometheus sample pair")
			}
			tm, err := sampleTime(pair[0])
			if err != nil {
				return Report{}, err
			}
			var text string
			if len(pair[1]) == 0 || pair[1][0] != '"' || json.Unmarshal(pair[1], &text) != nil {
				return Report{}, errors.New("invalid Prometheus sample value")
			}
			value, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return Report{}, errors.New("invalid Prometheus float value")
			}
			samples = append(samples, Sample{Time: tm, Value: value})
		}
		summary, err := Summarize(samples, start, end, step)
		if err != nil {
			return Report{}, err
		}
		key, _ := json.Marshal(labels)
		fraction, exponent := magnitude(summary)
		rows = append(rows, ranked{series: Series{Labels: labels, Summary: summary}, key: string(key), fraction: fraction, exponent: exponent})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].exponent != rows[j].exponent {
			return rows[i].exponent > rows[j].exponent
		}
		if rows[i].fraction != rows[j].fraction {
			return rows[i].fraction > rows[j].fraction
		}
		if rows[i].key != rows[j].key {
			return rows[i].key < rows[j].key
		}
		aJSON, _ := json.Marshal(rows[i].series.Summary)
		bJSON, _ := json.Marshal(rows[j].series.Summary)
		return string(aJSON) < string(bJSON)
	})
	for i, row := range rows {
		if i >= top {
			break
		}
		report.Series = append(report.Series, row.series)
	}
	report.OmittedSeries = len(rows) - len(report.Series)
	if ctx.Err() != nil {
		return Report{}, ctx.Err()
	}
	return report, nil
}

func sampleTime(raw json.RawMessage) (time.Time, error) {
	bad := errors.New("invalid Prometheus sample timestamp (requires numeric Unix seconds at nanosecond precision)")
	if len(raw) == 0 || len(raw) > 64 || raw[0] == '"' {
		return time.Time{}, bad
	}
	var number json.Number
	if json.Unmarshal(raw, &number) != nil || number == "" {
		return time.Time{}, bad
	}
	// Bound exact arithmetic before constructing a rational. Even a tiny JSON
	// number can otherwise request an enormous power-of-ten allocation.
	if at := strings.IndexAny(string(number), "eE"); at >= 0 {
		exponent, err := strconv.Atoi(string(number)[at+1:])
		if err != nil || exponent < -32 || exponent > 32 {
			return time.Time{}, bad
		}
	}
	r, ok := new(big.Rat).SetString(string(number))
	if !ok {
		return time.Time{}, bad
	}
	r.Mul(r, big.NewRat(int64(time.Second), 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return time.Time{}, bad
	}
	return time.Unix(0, r.Num().Int64()).UTC(), nil
}

// Present brackets all external labels and annotations with a fresh boundary.
func Present(report Report) (Report, error) {
	wrapper, err := safety.NewWrapper()
	if err != nil {
		return Report{}, err
	}
	boundary := strings.Split(wrapper.Wrap("Prometheus summaries", ""), "\n")
	report.Begin, report.Finish = boundary[0], boundary[2]
	return report, nil
}
