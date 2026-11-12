# Narrative says fixed

Run: run
Through sequence: 9
Status: unresolved

## Summary

Problem: Narrative says fixed
Validation outcome: applied\_but\_unverified
Unresolved reason: latency: no current validation recorded

## Impact and start time

Impact: Checkout requests were slow
Start time: 2026-11-12T10:00:00Z
Detection: Latency alert

## Timeline

| Time (UTC) | Entry | Event | Source |
| :--- | :--- | :--- | :--- |
| 2026-11-12T10:00:00Z | latency | Evidence: Queue grew after rollout | kubectl get pods -n checkout |
| 2026-11-12T10:01:00Z | action | remediation (applied): Workers restored | kubectl apply -f workers.yaml |

## Root cause

- cause: Worker concurrency was reduced
  Mechanism: Fewer workers increased queue time
  Supporting evidence: latency

## Decisive evidence

- latency: Queue grew after rollout
  Source: kubectl get pods -n checkout

## Fix and rollback

Validated fix: not recorded
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
  Observed: not recorded
  Verdict: unknown
  Validation: not recorded
  Source: not recorded
  Observed at: not recorded
  Window start: not recorded
  Window end: not recorded
  Reason: no current validation recorded

## Hypotheses ruled out

- rejected (ruled\_out): Database saturation
  Mechanism: Database latency stayed normal
  Contradicting evidence: latency
  Discriminating test: compare database latency

## Links

- https&#58;//example.test/incident
- https&#58;//example.test/dashboard
