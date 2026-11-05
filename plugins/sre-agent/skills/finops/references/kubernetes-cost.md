# Kubernetes cost

Cloud billing says what the provider charged. OpenCost and Kubecost help explain which workloads
occupied the resources. Used only when found, and read-only through the current kubectl context.

## Routes

Identify product, edition, and version before any request. Discovery reads them from deployment
images.

| Tool | Route | Parameters | Notes |
| :--- | :--- | :--- | :--- |
| OpenCost | `/allocation`, default port 9003 | `window`, `aggregate`, `accumulate`, `includeIdle`, `shareIdle` | [OpenCost API](https://opencost.io/docs/integrations/api/) |
| Kubecost self-hosted 1.x and 2.x | `/model/allocation`, default port 9090 | `window`, `aggregate`, `accumulate`, `idle`, `shareIdle` | [IBM Kubecost 2.x](https://www.ibm.com/docs/en/SSW0JQG_2.x/apis/monitoring-apis/api-allocation.html) |
| Kubecost 3.x, Kubecost Cloud, or an unidentified tool | | | Gap. Never guess an API |

```bash
scripts/finops_k8s.py allocation <run_dir> --tool opencost --tool-version 1.113.0 \
  --namespace opencost --service opencost --port 9003 \
  --window 2026-10-01T00:00:00Z,2026-11-01T00:00:00Z --aggregate namespace --include-idle
```

The script goes through the API server's service proxy, so no port-forward is left running.

## Figures

- EKS split cost allocation and GKE cost allocation come through billing data when enabled. Those
  figures are **measured**.
- OpenCost and Kubecost allocations aren't billing data, so those figures are **estimated**. The
  validator refuses a measured figure on allocation evidence.

## One ledger

Shown under Reconciliation when Kubernetes is in scope:

```text
workload allocations + shared + idle + unallocated + out-of-cluster + residual = covered provider charges
```

- Rows are kinds `workload`, `shared`, `idle`, `unallocated`, or `out_of_cluster`. A node-charge row
  is refused, because allocations must never be added on top of the same node charges.
- A residual is allowed, because cost tools can use list prices and telemetry differs from a delayed
  bill. Above 5% of covered charges, the report marks workload figures as unreconciled and adds a
  gap. The bill's own status is unaffected.
- Match on resource IDs, UTC windows, currency, and discount basis. Don't expect today's telemetry
  estimate to equal a delayed and reconciled bill.

[OpenCost specification](https://opencost.io/docs/specification/)

## Distributing a pool

Show the components first. If policy distributes shared and idle cost, keep the pools for drill-down
but mark them `distributed`, so they don't count. Distributing never changes the total.

With synthetic amounts: a cluster has $1,000 a day of covered cost, with $700 direct, $100 shared, and
$200 idle. Distributing the $300 pool 2:1 gives $200 and $100, and consolidated cost stays $1,000, not
$1,300.

```json
{
  "covered": "covered",
  "calc": "C20",
  "rows": [
    {"kind": "workload", "name": "direct", "amount": "direct", "distributed": false},
    {"kind": "shared", "name": "shared pool", "amount": "shared_pool", "distributed": true},
    {"kind": "idle", "name": "idle pool", "amount": "idle_pool", "distributed": true},
    {"kind": "workload", "name": "recipient a", "amount": "C21.share_1", "distributed": false},
    {"kind": "workload", "name": "recipient b", "amount": "C21.share_2", "distributed": false}
  ],
  "distributions": [{"pool": "pool_total", "calc": "C21", "from_rows": ["shared_pool", "idle_pool"]}]
}
```

`C20` is a `ledger` calculation over the rows that count, and `C21` is a `pool_distribution`. The
validator fails if a distributed pool still counts, if a share is missing, or if the rows differ from
the calculation's.

## Rightsizing

A rightsizing opportunity must cite telemetry evidence for representative peak usage, compared with
requests. Set `type` to `rightsizing` and `peak_evidence` to the evidence ID. Without peak evidence,
set `needs_investigation` and leave it out of the savings total. The validator enforces both.

Allocation follows the greater of request and use for many resources, so CPU utilization alone isn't
the bill.
