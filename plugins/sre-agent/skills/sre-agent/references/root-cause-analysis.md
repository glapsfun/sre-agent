# Root Cause Analysis

Phase 4 method. Inputs: the merged FACTS/ANOMALIES/GAPS blocks from every
dispatched investigator (the four core investigators, plus `sre-trace-analyst`
when a trace backend exists). Outputs: ranked hypotheses and the
expected-behavior criteria
that gate Phase 6. Interpretation happens here, in the main conversation —
investigators report facts only.

## Timeline first

Merge every timestamped fact into one ordered timeline before reasoning:

- first occurrence of each error signature (from `sre-logs-investigator`)
- alert `startsAt` times (from Alertmanager)
- metric inflection points (from the range queries)
- deploys, syncs, config revisions, image changes (from
  `sre-change-historian`)
- restart timestamps and event times (from `sre-k8s-investigator`)
- slowest-span locations, error-tagged spans, and exemplar trace
  timestamps (from `sre-trace-analyst`, when dispatched)

The root cause almost always **precedes** the first symptom. Anything that
happened after the first symptom is a consequence, not a cause — a common
trap is chasing a secondary failure (e.g. a dependency alarming because the
broken service stopped calling it).

## Changes-before-symptoms heuristic

Any change — deploy, config, image, infrastructure, dependency version,
traffic pattern — that landed in the window before the first symptom is the
**leading hypothesis** until evidence contradicts it. "Nothing was deployed"
widens the window: check infra changes, certificate expiries, data growth
crossing a threshold, cron jobs, and upstream/downstream services' changes.

## Hypothesis ranking

Record every hypothesis in the ledger with:

- **Statement** — one sentence.
- **Mechanism** — how exactly it produces the observed symptoms. No
  mechanism, no hypothesis.
- **Supporting evidence** — ledger entry numbers.
- **Contradicting evidence** — ledger entry numbers; be honest.
- **Confidence** — high: mechanism + ≥2 independent evidence sources;
  medium: mechanism + 1 source; low: plausible but unverified.
- **Discriminating test** — the cheapest read-only check that would confirm
  or refute it.

Run discriminating tests (read-only) before proposing remediations —
raising a hypothesis from medium to high with one query is always cheaper
than remediating the wrong cause.

## Five whys, bounded

### Durable collection budget

Use the existing `metadata` ledger kind to record round transitions. Round 0
contains the initial evidence waves; rounds 1 through 4 are the only additional
rounds. The database validates transitions and evidence references in the same
transaction as the append. This gate does not sandbox unrelated external tools:
the orchestrator must obtain a start receipt before dispatch and a finish receipt
before advancing. No stored check or recommendation is executed by the ledger.

Before initial collection, record this payload through `sre ledger add metadata
--run <run-id> --id <stable-start-id> --file <payload-file>` or `ledger_add`:

```json
{
  "investigation": {
    "number": 0,
    "state": "start",
    "reason": "Compare the plausible causes of the reported symptom",
    "checks": ["Collect scoped metrics, logs and change evidence"],
    "evidence_ids": [],
    "questions": ["Which observation distinguishes the current hypotheses?"]
  }
}
```

After synthesis, record `state: "finish"` for that same number, with an
`assessment` of `sufficient`, `insufficient`, or `no_useful_data`, a specific
reason, performed checks, actual same-run prior evidence IDs and unresolved
questions. Use empty arrays when there are no evidence IDs or questions. A
source failure is evidence with a nonempty `gap`; it is not useful support.
Never invent evidence IDs or copy the example's question without assessing it.

`sufficient` requires cited useful evidence and stops with `sufficient_evidence`.
It records the host's assessment, not proof of causality or a validated fix.
`no_useful_data` stops immediately. `insufficient` also stops if no cited useful
evidence was obtained; otherwise another round is eligible until round 4 ends
with `round_limit`. For additional rounds, useful evidence must be recorded
after the round's start, so stale evidence cannot justify continued collection.
Initial triage evidence may precede round 0. A new entry repeating an old fact
is not new information; the host must assess this honestly.

On resume, read `Investigation rounds` in the stored view. Resume an active
round; never increment it because a session was interrupted. Retrying the same
entry ID and payload preserves the original receipt and budget. Do not start
after a terminal stop, skip numbers, or automatically create another run to
restart the budget. A new scope needs an explicit operator decision and a link
to the prior run. Failed remediation does not reset the budget. When stopped,
report all current alternatives, missing observations, stop reason and unresolved
questions; propose resolutions and risks without executing them.

Iterate "why did that happen?" until you reach something actionable within
the team's control (a config value, a limit, a missing probe, a code path).
Stop when the next "why" leaves the technical domain (hiring, process,
vendor decisions) — record those as follow-up recommendations in the final
report, not as investigation branches.

## Common cloud-native failure chains

| Symptom | Typical chains | Discriminate by |
| :--- | :--- | :--- |
| OOMKilled | limit too low for legitimate load vs memory leak vs traffic spike | memory slope: leak climbs steadily regardless of traffic; spike correlates with RPS; chronic tightness OOMs at steady usage near limit |
| CrashLoopBackOff | config error vs bad image vs failing dependency vs liveness probe killing a slow-starting app | exit code (1/2 config or app, 137 kill), `--previous` logs, probe-failure events, whether the previous image version starts |
| Latency spike | CPU throttling/saturation vs downstream dependency vs retry storm vs GC pressure | throttling query; dependency latency vs own latency; request amplification (traffic up × errors up = retries); GC logs |
| Missing metrics | scrape target down vs ServiceMonitor label mismatch vs relabeling drop vs metric renamed vs cardinality limit | `up{}` for the target; ServiceMonitor selector vs Service labels; `/api/v1/targets` errors; app's `/metrics` endpoint directly |
| Pending pods | insufficient resources vs unsatisfiable affinity/taints vs unbound PVC vs quota | `kubectl describe pod` scheduling events name the failed predicate explicitly |
| Intermittent 5xx | one bad replica vs node-local issue vs race under load vs dependency flapping | error distribution per pod (`sum by (pod)`); per-node grouping; correlation with traffic level |

## Defining expected behavior

Turn "fixed" into measurable criteria before proposing any fix — this is the
TDD failing test. Each criterion needs a metric/observation, a threshold,
and a window:

```text
Expected behavior:
  - error rate < 1% of requests          (PromQL from prometheus-analysis.md)
  - p99 latency < 300ms                  (same source as the evidence query)
  - container restarts = 0               (kube_pod_container_status_restarts_total)
  - no new ERROR-class log signatures    (LogQL error hunt)
  - alert <name> resolved                (Alertmanager API)
Observation window: 15 minutes (default) — use ≥ 3× the symptom period for
intermittent issues.
```

Use the **same queries** that established the evidence, so before/after is
apples-to-apples. These criteria go in the ledger verbatim and become Phase
6's validation checklist.
