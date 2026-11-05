# GCP billing

## Where the numbers come from

| Need | Preferred | Fallback | If neither |
| :--- | :--- | :--- | :--- |
| Control total | Invoice figure from the user, with `finops_record.py provided` | | Status "Not reconciled: no invoice figure" |
| Line-item breakdown | Billing export in BigQuery, through `finops_gcp.py query` | | Gap. **No spend numbers** |
| Opportunities | `finops_gcp.py recommendations` | | Only opportunities derived from evidence |
| CUDs | `sql/gcp-cud.sql`, program and migration date established first | | Gap |
| Telemetry | `finops_gcp.py monitoring`, Prometheus | | Cause recorded as not established |

## With no export

GCP's Cloud Billing API has no method that returns incurred cost, so without an export the skill
can't report GCP spend. Say that plainly. Don't estimate spend from SKU prices and usage.

A human enables an export under Billing export in the Cloud Billing console. Don't promise history
from before enablement: it depends on the dataset's location and on service coverage.

[Export overview and limitations](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery)

## Prepared queries

All are in `sql/`. Read the schema first with `finops_gcp.py schema`, and run them with
`finops_gcp.py query`. `table` is the full export table, such as
`proj.billing.gcp_billing_export_v1_0123AB`.

| File | Parameters | Columns | Answers |
| :--- | :--- | :--- | :--- |
| `sql/gcp-invoice-total.sql` | `table`, `invoice_month` | `invoice_month,currency,cost,credits,net_cost` | What do we owe? |
| `sql/gcp-drivers.sql` | `table`, `invoice_month` | `invoice_month,service,sku,location,currency,usage_amount,usage_unit,cost,credits,net_cost` | What moved? |
| `sql/gcp-effective.sql` | `table`, `usage_start`, `usage_end` | `usage_month,currency,effective_cost` | What did it consume? |
| `sql/gcp-no-owner.sql` | `table`, `invoice_month`, `owner_label` | `invoice_month,currency,no_owner_cost` | What has no owner? |
| `sql/gcp-freshness.sql` | `table`, `invoice_month` | `max_usage_end,max_export_time` | How current is the data? |
| `sql/gcp-cud.sql` | `table`, `first_month`, `second_month` | `invoice_month,usage_cost,credits,fee_cost,net_cost` | What does a commitment really cost? |
| `sql/gcp-bq-job-cost.sql` | `table`, `start_date`, `end_date` | `user_email,total_bytes_billed,total_slot_ms` | What did the analysis itself cost? |

For `sql/gcp-bq-job-cost.sql`, `table` is `region-us.INFORMATION_SCHEMA.JOBS`, with your region.

```bash
scripts/finops_gcp.py estimate <run_dir> --project proj --location US \
  --query-file sql/gcp-invoice-total.sql --param table=proj.billing.gcp_billing_export_v1_0123AB --param invoice_month=2026-10
scripts/finops_gcp.py query <run_dir> --project proj --location US \
  --query-file sql/gcp-invoice-total.sql --param table=proj.billing.gcp_billing_export_v1_0123AB --param invoice_month=2026-10 \
  --source "BigQuery, invoice total" --basis billed
```

## Cost control

`finops_gcp.py estimate` is a dry run, which reports bytes without running or billing. `query` dry
runs first, refuses if the bytes exceed what remains, and then runs with `--maximum_bytes_billed`
set to what remains. Over that limit, the query fails without charge. It also sets `--max_rows`,
because `bq` otherwise returns only 100 rows and would silently undercount a bill.

A `LIMIT` doesn't reduce a scan on a non-clustered table. The prepared queries filter on
`invoice.month`, which doesn't prune partitions, so check the dry-run bytes on large exports.

On capacity pricing, the bytes limit has no direct dollar meaning, and the script records a gap
saying so.

[BigQuery cost controls](https://docs.cloud.google.com/bigquery/docs/best-practices-costs)

## Accounting traps

- **`UNNEST` multiplies cost.** A row with several credits or labels becomes several rows, and
  joining both arrays multiplies again. Aggregate credits inside each original row before grouping,
  as the prepared queries do.
- **Two time axes.** Use `invoice.month` to reconcile and `usage_start_time` to study when usage
  happened. Late usage lands on a later invoice, so filtering an invoice query by usage month
  silently omits charges.
- **Floats.** `cost` and `credits.amount` are floats. The prepared queries cast to `NUMERIC` before
  summing, as Google's examples do.
- **Keep the odd rows visible.** Tax, adjustments, rounding, commitment fees, and charges with no
  project all count toward the invoice.
- **CUD programs differ.** Newer spend-based programs use discounted prices and fee offsets where
  older ones used list prices and credits. Establish the program and migration date, and compare net
  cost: less credit volume is not lost savings. Check the fee SKU names and credit types in
  `sql/gcp-cud.sql` against real data. [Spend-based CUD models](https://docs.cloud.google.com/docs/cuds-multiprice)
- **BigQuery's own cost.** Parent `SCRIPT` jobs double-count their children, and slot milliseconds
  measure consumption, not an amount owed. [JOBS view](https://docs.cloud.google.com/bigquery/docs/information-schema-jobs)
- **GKE allocation** needs cost allocation enabled and the detailed export, has no backfill, and
  allocates by request. Combine it with utilization telemetry when judging requests.
  [GKE cost allocation](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/cost-allocations)

[Detailed export schema](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery-tables/detailed-usage),
[Google's query examples](https://docs.cloud.google.com/billing/docs/how-to/bq-examples)
