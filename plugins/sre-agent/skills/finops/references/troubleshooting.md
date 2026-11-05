# Troubleshooting

These are hypotheses to test, not findings about a particular account.

| Symptom | Likely explanation to test | Evidence or check |
| :--- | :--- | :--- |
| Latest day is unusually cheap | Delayed or incomplete export | Freshness query, export refresh time, completeness against prior days |
| GCP SQL exceeds invoice | Cost multiplied by credit or label joins | Control total before `UNNEST`, and row-level credit aggregation |
| GCP SQL misses invoice charges | Usage month confused with invoice month | Compare `invoice.month` with late usage and adjustment rows |
| AWS daily cost spikes at purchase | Cash and amortized views mixed | Charge types, upfront purchases, effective-cost view |
| Commitment savings appear to vanish | CUD model or accounting changed | Migration date, consumption model, fee SKUs, signed offsets |
| Node removal predicts savings but the bill stays flat | Commitments remain payable, or capacity is replaced | Hourly commitment use, replacement nodes, actual net charges |
| Namespace totals exceed the cluster bill | Parent costs or redistributed pools added twice | The one ledger, and pool conservation |
| Kubecost disagrees with billing | List pricing, stale reconciliation, or unmatched resource IDs | Pricing basis, integration status, complete-day comparison |
| Kubernetes idle becomes negative | Duplicate samples, bad joins, or mismatched capacity windows | Scrape targets, IDs, allocation intervals, provider prices |
| A team shows no cost | Missing labels, export coverage, or access scope | No-owner row, activation date, resource IDs, query permissions |
| Athena returns no current partitions | Export, crawler, or table location mismatch | Latest report manifest, S3 path, Glue schema and partitions |
| Athena access denied | A separate S3, Glue or Lake Formation, Athena, or KMS gate | The exact denied operation, and scoped identity permissions |
| BigQuery table not found | Wrong project, dataset, export type, or location | Table metadata and job location |
| A query costs more than expected | Wide scan despite `LIMIT`, or a wrong partition filter | Dry-run bytes, actual partitions, selected columns |
| BigQuery slot analysis overcounts | `SCRIPT` parent and child jobs both summed | Job IDs, `parent_job_id`, `statement_type` |
| An API request returns an empty result | Wrong product API, auth, retention, filters, or incomplete telemetry | Product and version, HTTP and response errors, requested versus returned windows |
| `FIGURE_MISMATCH` on a figure you just read | The value was copied wrong, or the raw result changed | The raw result at the figure's locator |
| `LOCATOR_FAILED` on a CSV figure | The `where` matches no row or several | Add the columns that make the row unique |
| Status is "Not reconciled" by a small amount | Rounding, a Marketplace invoice left out, or an after-tax invoice figure | Sum all invoices for the period, and compare before tax |
| `RECON_ROLE` on a figure that looks right | It came from the wrong place: another period, a calculation, or query results where an invoice belongs | The evidence entry's `role` and `period` |
| `BUDGET:` says a query's use is unknown | An earlier query outlived its status check | Wait for it and rerun. Don't start it again by hand |

[GCP schema and timing](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery-tables/detailed-usage),
[AWS export processing](https://docs.aws.amazon.com/cur/latest/userguide/dataexports-processing.html),
[OpenCost troubleshooting](https://opencost.io/docs/troubleshooting/),
[BigQuery query troubleshooting](https://docs.cloud.google.com/bigquery/docs/troubleshoot-queries)

## Budgets and alerts aren't brakes

AWS Cost Anomaly Detection uses delayed Cost Explorer data, so it isn't a real-time kill switch. On GCP, alerts-only budgets don't cap
usage, and spend-cap budgets apply only to eligible services, using gross estimated costs. Check eligibility and business impact before
proposing enforcement.

[AWS anomaly detection timing](https://docs.aws.amazon.com/cost-management/latest/userguide/manage-ad.html),
[GCP budgets](https://docs.cloud.google.com/billing/docs/how-to/budgets),
[GCP spend-cap budgets](https://docs.cloud.google.com/billing/docs/how-to/budgets-spend-caps)
