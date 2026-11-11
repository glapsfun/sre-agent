# Prometheus Analysis

For compact range summaries, use the CLI when available:

```bash
sre prom query 'up{namespace="payments"}' --context production --range 1h --step 30s --top 20
sre prom query 'up' --url http://localhost:9090 --format text
```

Defaults: range `1h`, step `30s`, top `20`, and `--timeout 30s` for the complete
operation. CLI durations and top must be positive. The range ends at the
current time, rounded down to a millisecond. The explicit URL is a base URL;
deployment prefixes such as `https://metrics.example.com/prometheus` are
preserved when appending `/api/v1/query_range`.

Without `--url`, context precedence is `--context`, `SRE_KUBE_CONTEXT`, then
one `kubectl config current-context` read. The selected context is pinned for
discovery and the authenticated Kubernetes service-proxy query. Automatic
selection requires a complete service-endpoint inventory and exactly one
Prometheus service. Exposed port 9090 wins; otherwise the service must expose
one distinct port. No port-forward is started, and readiness URLs from
discovery are forwarding hints, not verified listeners. Missing/ambiguous
endpoints, incomplete inventory, skipped cluster discovery, and proxy access
or transport failures require `--url`. Use an explicit URL for TLS-only
services or services with a custom API prefix.

JSON and text reports list each selected series' redacted labels and finite
first, last, minimum, maximum, and arithmetic mean. The first and last are the
first and last finite samples. NaNs and positive/negative infinities are
counted separately and excluded from all numeric statistics. All-nonfinite
and empty series have null statistics. Missing steps include both window
edges and gaps inside the evaluation grid; absent data is never zero.
Duplicate, unordered, off-grid, or out-of-window samples fail the query.

A change point minimizes the sum of squared errors of two constant levels
over the finite samples, with at least two finite samples on each side. The
split needs at least 20% less error than the single-level fit and must exceed
a numerical noise floor of `1e-12 * finite sample count` after power-of-two
normalization. Tied splits select the earliest time. The reported time is the
first finite sample of the second segment. This is a descriptive heuristic:
it can identify gradual trends and can span missing data. It is not a
statistical significance test, an alert threshold, or proof of cause.

Ranking uses the absolute difference between the two fitted levels when a
change exists, otherwise the absolute finite last-minus-first value. Canonical
redacted-label JSON breaks ties, with summary JSON breaking any remaining
redaction collision. `omitted_series` counts every series beyond `--top`.
Raw samples are never written to storage. Labels and info annotations appear
inside fresh external-data markers; API errors and warnings are redacted and
marked too. Warnings fail the query even with HTTP 200; info annotations are
retained. No failed query returns a successful empty or partial report.

Responses are limited to 8 MiB. Matrix series are decoded separately, with a
65,536 JSON structural-token limit per series or metadata field and explicit
errors on excess. This admits 500 series over the default one-hour, 30-second
grid without raising discovery's response-work limit. Numeric sample timestamps support exact nanosecond Unix
times within the int64 nanosecond range. Native histogram samples are rejected;
use a float conversion query such as `histogram_sum()`, `histogram_count()`, or
`histogram_avg()` before summarizing them.

The [Prometheus HTTP API](https://prometheus.io/docs/prometheus/latest/querying/api/)
defines matrix responses and query-range parameters. The
[querying basics](https://prometheus.io/docs/prometheus/latest/querying/basics/)
describe equally spaced range evaluations and float/native histogram samples.
The [Kubernetes service-proxy documentation](https://kubernetes.io/docs/tasks/access-application-cluster/access-cluster-services/)
describes authenticated service access through the API server.

## Curl fallback

How to query Prometheus during an investigation. Reach the API through the
port-forward from `discovery.md` (`kubectl port-forward -n <ns> svc/<name>
9090:9090`), then query with `curl -fsS`:

```bash
# Instant query
curl -fsS -G 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=<promql>'

# Range query (history around the incident)
curl -fsS -G 'http://localhost:9090/api/v1/query_range' \
  --data-urlencode 'query=<promql>' \
  --data-urlencode 'start=2026-07-03T08:00:00Z' \
  --data-urlencode 'end=2026-07-03T11:00:00Z' \
  --data-urlencode 'step=60s'
```

Substitute `$NS` and `$WORKLOAD` below with the investigation scope. Label
names vary per app (`job`, `service`, `pod`, custom) — verify with the metric
discovery section before trusting an empty result.

## Golden signals

```promql
# Traffic (RPS)
sum(rate(http_requests_total{namespace="$NS", pod=~"$WORKLOAD.*"}[5m]))

# Error rate (fraction of 5xx)
sum(rate(http_requests_total{namespace="$NS", pod=~"$WORKLOAD.*", code=~"5.."}[5m]))
  / sum(rate(http_requests_total{namespace="$NS", pod=~"$WORKLOAD.*"}[5m]))

# p99 latency (classic histogram)
histogram_quantile(0.99, sum by (le)
  (rate(http_request_duration_seconds_bucket{namespace="$NS", pod=~"$WORKLOAD.*"}[5m])))

# CPU usage vs requests
sum(rate(container_cpu_usage_seconds_total{namespace="$NS", pod=~"$WORKLOAD.*", container!=""}[5m]))
sum(kube_pod_container_resource_requests{namespace="$NS", pod=~"$WORKLOAD.*", resource="cpu"})

# Memory working set as % of limit, per pod
max by (pod) (container_memory_working_set_bytes{namespace="$NS", pod=~"$WORKLOAD.*", container!=""}
  / on (pod, container) kube_pod_container_resource_limits{resource="memory"} * 100)

# CPU throttling (fraction of periods throttled — >0.25 is significant)
sum(rate(container_cpu_cfs_throttled_periods_total{namespace="$NS", pod=~"$WORKLOAD.*"}[5m]))
  / sum(rate(container_cpu_cfs_periods_total{namespace="$NS", pod=~"$WORKLOAD.*"}[5m]))
```

Error-status label is `code` in some instrumentations and `status` in others;
gRPC uses `grpc_server_handled_total{grpc_code!="OK"}`.

## Kubernetes health (kube-state-metrics)

```promql
# Pods in non-Running, non-Succeeded state
kube_pod_status_phase{namespace="$NS", phase!="Running", phase!="Succeeded"} == 1

# Restart rate (restarts in the last hour)
increase(kube_pod_container_status_restarts_total{namespace="$NS", pod=~"$WORKLOAD.*"}[1h])

# OOMKilled containers in the last hour
increase(kube_pod_container_status_last_terminated_reason{namespace="$NS", reason="OOMKilled"}[1h]) > 0

# Pods not ready
kube_pod_status_ready{namespace="$NS", condition="false"} == 1

# Deployment: unavailable replicas
kube_deployment_status_replicas_unavailable{namespace="$NS", deployment="$WORKLOAD"} > 0

# Deployment: update stuck (generation mismatch)
kube_deployment_status_observed_generation{namespace="$NS", deployment="$WORKLOAD"}
  != kube_deployment_metadata_generation{namespace="$NS", deployment="$WORKLOAD"}

# HPA pinned at max replicas (scaling ceiling hit)
kube_horizontalpodautoscaler_status_current_replicas{namespace="$NS"}
  == kube_horizontalpodautoscaler_spec_max_replicas{namespace="$NS"}

# Node pressure conditions
kube_node_status_condition{condition=~"MemoryPressure|DiskPressure", status="true"} == 1
```

## Baseline comparison

An absolute number means little without a baseline. Two techniques:

```promql
# Same query, same time yesterday. The offset modifier must sit on the range
# vector selector, not on the aggregation — `sum(...) offset 1d` is a parse error.
sum(rate(http_requests_total{namespace="$NS", code=~"5.."}[5m] offset 1d))
```

And a range query spanning incident start − 2h to now (`query_range` with
`step=60s`) to see exactly when the curve broke.

A deviation is meaningful when it is **> 2× the baseline** or crosses a known
SLO bound — not when it wiggles within normal variance. Report the deviation
factor, the first timestamp where it exceeds the threshold, and hand that
timestamp to the timeline in `root-cause-analysis.md`.

## Metric discovery when names are unknown

```bash
# Is the target scraped at all? (the first question when "metrics disappeared")
curl -fsS -G 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=up{namespace="$NS"}'

# All metric names for the namespace
curl -fsS -G 'http://localhost:9090/api/v1/label/__name__/values' \
  --data-urlencode 'match[]={namespace="$NS"}'

# Prove a metric is absent (returns 1 when the series does not exist)
curl -fsS -G 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=absent(http_requests_total{namespace="$NS"})'

# Scrape target status with error messages
curl -fsS 'http://localhost:9090/api/v1/targets' | head -c 2000
```

On Prometheus-Operator clusters, the scrape config comes from
ServiceMonitor/PodMonitor objects:

```bash
kubectl get servicemonitors,podmonitors -n $NS
```

A missing metric is usually one of: target down (`up == 0`), no
ServiceMonitor label match, relabeling dropped it, or the app stopped
exposing it (check `curl <pod-ip>:<metrics-port>/metrics` via
`kubectl exec`).

## SLO burn rate

Burn rate = observed error ratio ÷ error budget. For a 99.9% SLO the budget
is 0.001; the standard multiwindow pattern (Google SRE Workbook) pages when
both a long and a short window burn fast:

```promql
# Fast burn — pages: 14.4x budget over 1h AND 5m (2% of monthly budget in 1h)
(
  sum(rate(http_requests_total{namespace="$NS", code=~"5.."}[1h]))
    / sum(rate(http_requests_total{namespace="$NS"}[1h])) > (14.4 * 0.001)
and
  sum(rate(http_requests_total{namespace="$NS", code=~"5.."}[5m]))
    / sum(rate(http_requests_total{namespace="$NS"}[5m])) > (14.4 * 0.001)
)

# Slow burn — tickets: 6x budget over 6h AND 30m
(
  sum(rate(http_requests_total{namespace="$NS", code=~"5.."}[6h]))
    / sum(rate(http_requests_total{namespace="$NS"}[6h])) > (6 * 0.001)
and
  sum(rate(http_requests_total{namespace="$NS", code=~"5.."}[30m]))
    / sum(rate(http_requests_total{namespace="$NS"}[30m])) > (6 * 0.001)
)
```

During an incident: a fast-burn condition means mitigate now; a slow-burn
condition means investigate but the budget can absorb some deliberation.

## Alertmanager

```bash
# Active alerts for the namespace (port-forward Alertmanager on 9093 first)
curl -fsS 'http://localhost:9093/api/v2/alerts?filter=namespace%3D%22'$NS'%22'
```

For each active alert record: name, `startsAt`, and labels. The alert start
time is often the most precise "first symptom" timestamp available —
correlate it with the change timeline from the `sre-change-historian`
findings.
