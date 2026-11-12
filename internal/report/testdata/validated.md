# Checkout latency

Run: run
Through sequence: 10
Status: resolved

## Summary

Problem: Checkout latency
Validation outcome: resolved

## Impact and start time

Impact: Checkout requests were slow
Start time: 2026-11-12T10:00:00Z
Detection: Latency alert

## Timeline

| Time (UTC) | Entry | Event | Source |
| :--- | :--- | :--- | :--- |
| 2026-11-12T10:00:00Z | latency | Evidence: Queue grew after rollout | kubectl get pods -n checkout |
| 2026-11-12T10:01:00Z | action | remediation (applied): Workers restored | kubectl apply -f workers.yaml |
| 2026-11-12T10:02:00Z | healthy | Evidence: p99 recovered | prom query checkout\_latency |
| 2026-11-12T11:00:00Z | validation | Validation latency (pass): 0.4s | prom query checkout\_latency |

## Root cause

- cause: Worker concurrency was reduced
  Mechanism: Fewer workers increased queue time
  Supporting evidence: latency

## Decisive evidence

- latency: Queue grew after rollout
  Source: kubectl get pods -n checkout
- healthy: p99 recovered
  Source: prom query checkout\_latency

## Fix and rollback

Validated fix: action
Selected action: action

- action: remediation (applied)
  Command: kubectl apply -f workers.yaml
  Result: Workers restored
  Option: option; Restore worker concurrency
  Approval: approval
  Rollback: kubectl rollout undo deploy/checkout

## Validation results

Expected behavior: expected

- latency: Checkout latency recovers
  Observation: p99 latency
  Threshold: below 1s
  Criterion source: prom query checkout\_latency
  Declared window: 60 seconds
  Observed: 0.4s
  Verdict: pass
  Validation: validation
  Source: prom query checkout\_latency
  Observed at: 2026-11-12T11:00:00Z
  Window start: 2026-11-12T10:01:00Z
  Window end: 2026-11-12T10:02:00Z
  Reason: not recorded

## Hypotheses ruled out

- rejected (ruled\_out): Database saturation
  Mechanism: Database latency stayed normal
  Contradicting evidence: latency
  Discriminating test: compare database latency

## Links

- https&#58;//example.test/incident
- https&#58;//example.test/dashboard
