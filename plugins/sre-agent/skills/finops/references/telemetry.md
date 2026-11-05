# Telemetry and change evidence

Billing says what moved. Telemetry and deploy history test why. A cause is only stated when evidence supports it.

## Recording evidence

`scripts/finops_capture.py` runs one read-only command from an allowlist and saves its output as evidence. It refuses
anything else before it runs, including reading Kubernetes secrets.

```bash
# NAT traffic for a suspected driver
scripts/finops_capture.py <run_dir> --source "CloudWatch NAT bytes" --kind telemetry -- \
  aws cloudwatch get-metric-statistics --namespace AWS/NATGateway --metric-name BytesOutToDestination \
  --dimensions Name=NatGatewayId,Value=nat-123 --start-time 2026-10-01T00:00:00Z --end-time 2026-11-01T00:00:00Z \
  --period 86400 --statistics Sum

# Peak memory for a rightsizing check, through a Prometheus service
scripts/finops_capture.py <run_dir> --source "Prometheus search pool peak" --kind telemetry -- \
  kubectl get --raw "/api/v1/namespaces/monitoring/services/prometheus:9090/proxy/api/v1/query?query=max_over_time(container_memory_working_set_bytes%7Bnamespace%3D%22search%22%7D%5B30d%5D)"

# What was deployed, and when
scripts/finops_capture.py <run_dir> --source "orders-api deploy history" --kind change -- \
  kubectl rollout history deploy/orders-api -n orders
scripts/finops_capture.py <run_dir> --source "orders-api commits" --kind change -- git log --since=2026-10-01 --oneline
```

Also allowed: `kubectl get`, `describe`, and `top`; `git show`; `helm history`; `flux get`; `argocd app history`; `gh pr list`,
`pr view`, `run list`, and `run view`; `aws cloudwatch get-metric-data`; `aws cloudtrail lookup-events`; `gcloud logging read`.

On GCP, use `scripts/finops_gcp.py monitoring` for Cloud Monitoring.

## What each kind can do

| Evidence kind | Can back a figure? | Can support a cause? | Can prove a peak? |
| :--- | :--- | :--- | :--- |
| `telemetry` | Yes, as measured, such as a usage count | Yes | Yes |
| `change` | No | Yes | No |
| `reported` | No | Yes, and the report names who reported it | No |

## Peaks

Investigate representative peaks and scheduling constraints before proposing lower requests or fewer nodes. Averages hide what
capacity is for. Lower spend caused by failed transactions isn't an efficiency improvement, so check the service's reliability
target too.

## Using sre-agent

When sre-agent's subagents or playbooks are available, `sre-metrics-analyst` and `sre-change-historian` can test why a driver moved.
They return findings, not raw results, so save those and record them:

```bash
scripts/finops_record.py reported <run_dir> --agent sre-metrics-analyst --file findings.txt
```

That evidence can support a cause statement, marked with the agent's name, but can't back a money figure or prove a peak. Anything a
number depends on comes through `scripts/finops_capture.py`.

This skill works without sre-agent installed. Without those subagents, run the commands above yourself.
