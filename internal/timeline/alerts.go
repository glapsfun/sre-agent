package timeline

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/safety"
)

const maxAlertResponseBytes = 16 << 20

type alertRecord struct {
	Fingerprint string            `json:"fingerprint"`
	StartsAt    string            `json:"startsAt"`
	EndsAt      string            `json:"endsAt"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Status      struct {
		State       string   `json:"state"`
		SilencedBy  []string `json:"silencedBy"`
		InhibitedBy []string `json:"inhibitedBy"`
	} `json:"status"`
}

// Custom HTTPDoers must honor request context and must not follow redirects.
// Standard clients are copied so this collector can enforce that policy without
// changing the caller's transport, credentials, cookie jar or redirect behavior.
func alertClient(client HTTPDoer) HTTPDoer {
	if client == nil {
		client = &http.Client{}
	}
	if standard, ok := client.(*http.Client); ok {
		copy := *standard
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return &copy
	}
	return client
}

func alertFailure(ctx context.Context, code, message string) SourceResult {
	if ctx.Err() != nil {
		code = "timeout"
		message = "Alertmanager collection deadline or cancellation reached"
	}
	return externalFailure("alertmanager", code, message)
}

func alertTransportFailure(ctx context.Context, err error, message string) SourceResult {
	code := "error"
	var timeout interface{ Timeout() bool }
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &timeout) && timeout.Timeout()) {
		code = "timeout"
	}
	return alertFailure(ctx, code, message)
}

func ParseAlertmanagerURL(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, errors.New("Alertmanager endpoint must be an HTTP(S) URL without userinfo or fragment")
	}
	return endpoint, nil
}

func CollectAlerts(ctx context.Context, scope Scope, client HTTPDoer) SourceResult {
	result := newKubeResult("alertmanager")
	if scope.AlertmanagerURL == "" {
		result.Summary.Status = "missing"
		sourceGap(&result, "missing", "no Alertmanager endpoint was supplied")
		return result
	}
	ctx, cancel := externalContext(ctx)
	defer cancel()
	if ctx.Err() != nil {
		return alertFailure(ctx, "timeout", "")
	}
	endpoint, err := ParseAlertmanagerURL(scope.AlertmanagerURL)
	if err != nil {
		return alertFailure(ctx, "error", err.Error())
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v2/alerts"
	if endpoint.RawPath != "" {
		endpoint.RawPath = strings.TrimRight(endpoint.RawPath, "/") + "/api/v2/alerts"
	}
	query := endpoint.Query()
	for _, key := range []string{"active", "silenced", "inhibited", "unprocessed"} {
		query.Set(key, "true")
	}
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return alertFailure(ctx, "error", "invalid Alertmanager request URL")
	}
	request.Header.Set("Accept", "application/json")
	response, err := alertClient(client).Do(request)
	if err != nil {
		return alertTransportFailure(ctx, err, "Alertmanager request failed")
	}
	if response == nil || response.Body == nil {
		return alertFailure(ctx, "error", "Alertmanager returned no response body")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		code := "error"
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			code = "denied"
		}
		return alertFailure(ctx, code, fmt.Sprintf("Alertmanager returned HTTP %d; response body omitted", response.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAlertResponseBytes+1))
	if err != nil {
		return alertTransportFailure(ctx, err, "Alertmanager response could not be read")
	}
	if len(body) > maxAlertResponseBytes {
		return alertFailure(ctx, "error", "Alertmanager response exceeds 16 MiB limit")
	}
	parser := &listReader{ctx: ctx, input: bufio.NewReader(strings.NewReader(string(body)))}
	err = parser.items(func(raw json.RawMessage) error {
		result.Summary.Observed++
		var alert alertRecord
		if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &alert) != nil {
			return errors.New("invalid alert fields")
		}
		matched, gap := alertMatches(scope, alert.Labels)
		if gap != "" {
			sourceGap(&result, "association", gap)
		}
		if !matched {
			return nil
		}
		result.Summary.Matched++
		start, e1 := time.Parse(time.RFC3339Nano, alert.StartsAt)
		end, e2 := time.Parse(time.RFC3339Nano, alert.EndsAt)
		if e1 != nil || e2 != nil || start.IsZero() || end.IsZero() || end.Before(start) {
			sourceGap(&result, "timestamp", "alert has missing or invalid firing interval timestamps")
			return nil
		}
		if start.After(scope.End) || !end.After(scope.End) {
			return nil
		}
		if start.Before(scope.Start) {
			if result.Summary.Context == nil {
				result.Summary.Context = map[string]any{"firing_before_window": 0}
			}
			result.Summary.Context["firing_before_window"] = result.Summary.Context["firing_before_window"].(int) + 1
			return nil
		}
		entry, err := projectAlert(ctx, alert, start.UTC(), end.UTC(), endpoint.EscapedPath())
		if err != nil {
			return err
		}
		result.Entries = append(result.Entries, entry)
		return ctx.Err()
	})
	if err == nil {
		if e := parser.space(); e != io.EOF {
			err = errors.New("trailing alert response data")
		}
	}
	if err != nil {
		return alertFailure(ctx, "error", "invalid or incomplete Alertmanager JSON response; no records retained")
	}
	if ctx.Err() != nil {
		return alertFailure(ctx, "timeout", "")
	}
	sourceGap(&result, "history", "current firing alerts cannot reconstruct resolved alerts or complete historical coverage")
	sourceGap(&result, "redacted", "alert output includes only identity labels, alertname, severity, scrape job, summary and description; other labels, annotations and URLs are omitted")
	return finishKubeResult(result)
}

var alertIdentityLabels = []string{"namespace", "kubernetes_namespace", "workload", "workload_type", "controller", "controller_kind", "pod", "kubernetes_pod_name", "deployment", "statefulset", "daemonset", "cronjob", "replicaset"}

func alertMatches(scope Scope, labels map[string]string) (bool, string) {
	namespace := labels["namespace"]
	if other := labels["kubernetes_namespace"]; other != "" {
		if namespace != "" && namespace != other {
			return false, "alert namespace labels conflict"
		}
		namespace = other
	}
	if namespace == "" {
		return false, "alert has no namespace identity label"
	}
	if namespace != scope.Namespace {
		return false, ""
	}
	if scope.Workload == "" {
		return true, ""
	}
	if scope.Target == nil {
		return false, "requested workload identity is unresolved; alert association unavailable"
	}
	type identity struct{ kind, name string }
	var identities []identity
	for _, pair := range [][2]string{{"workload", "workload_type"}, {"controller", "controller_kind"}} {
		name, kind := labels[pair[0]], labels[pair[1]]
		if name == "" && kind != "" {
			return false, "alert workload type has no corresponding name"
		}
		if name != "" {
			if kind != "" {
				kind = workloadKinds[strings.ToLower(kind)]
				if kind == "" {
					return false, "alert workload kind is unsupported"
				}
			}
			identities = append(identities, identity{kind, name})
		}
	}
	// Prometheus job identifies the scrape job, not a Kubernetes Job. Jobs
	// require an explicit workload/type or controller/kind pair instead.
	for _, label := range []string{"pod", "kubernetes_pod_name", "deployment", "statefulset", "daemonset", "cronjob", "replicaset"} {
		if name := labels[label]; name != "" {
			kind := workloadKinds[label]
			if label == "kubernetes_pod_name" {
				kind = "Pod"
			}
			identities = append(identities, identity{kind, name})
		}
	}
	if len(identities) == 0 {
		return false, "alert has no supported workload identity label"
	}
	matched, unmatched := false, false
	for i, id := range identities {
		for _, previous := range identities[:i] {
			if id.kind != "" && id.kind == previous.kind && id.name != previous.name {
				return false, "alert workload identity labels conflict"
			}
		}
		count, related := 0, false
		for uid, obj := range scope.Objects {
			if obj.Namespace == namespace && obj.Name == id.name && (id.kind == "" || obj.Kind == id.kind) {
				count++
				related = scope.Related[uid]
			}
		}
		if count > 1 {
			return false, "alert workload identity is ambiguous across resolved objects"
		}
		if count == 1 && related {
			matched = true
		} else {
			unmatched = true
		}
	}
	if matched && unmatched {
		return false, "alert workload identity labels conflict with resolved scope"
	}
	return matched && !unmatched, ""
}

func projectAlert(ctx context.Context, alert alertRecord, start, end time.Time, path string) (Entry, error) {
	var failure error
	text := func(value string) string {
		if failure != nil {
			return ""
		}
		var clean string
		clean, failure = safety.NewRedactor().RedactContext(ctx, value)
		return clean
	}
	labels := map[string]any{}
	for _, key := range append([]string{"alertname", "severity", "job"}, alertIdentityLabels...) {
		if value, ok := alert.Labels[key]; ok {
			labels[key] = text(value)
		}
	}
	details := map[string]any{"labels": labels, "state": text(alert.Status.State), "silenced": len(alert.Status.SilencedBy) > 0, "inhibited": len(alert.Status.InhibitedBy) > 0, "ends_at": end}
	for _, key := range []string{"summary", "description"} {
		if value, ok := alert.Annotations[key]; ok {
			details[key] = text(value)
		}
	}
	id := text(alert.Fingerprint)
	if id == "" {
		encoded, _ := json.Marshal(labels)
		digest := sha256.Sum256(append(encoded, []byte(start.Format(time.RFC3339Nano))...))
		id = hex.EncodeToString(digest[:])
	}
	name := text(alert.Labels["alertname"])
	namespace := alert.Labels["namespace"]
	if namespace == "" {
		namespace = alert.Labels["kubernetes_namespace"]
	}
	entry := Entry{ID: id, Time: start, TimeBasis: "startsAt", Source: "alertmanager", Object: Object{Kind: "Alert", Namespace: text(namespace), Name: name}, Change: "Firing alert: " + name, Command: "GET " + text(path) + "?active=true&inhibited=true&silenced=true&unprocessed=true", Details: details}
	return entry, failure
}
