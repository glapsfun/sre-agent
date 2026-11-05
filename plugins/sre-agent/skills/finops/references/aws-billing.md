# AWS billing

## Where the numbers come from

| Need | Preferred | Fallback | If neither |
| :--- | :--- | :--- | :--- |
| Control total | `finops_aws.py invoice` | Invoice figure from the user | Cross-check Cost Explorer against CUR |
| Line-item breakdown | CUR 2.0 or legacy CUR in Athena, through `finops_aws.py athena` | `finops_aws.py ce` | Gap. No spend numbers |
| Opportunities | `finops_aws.py recommendations` | `--source compute-optimizer` | Only opportunities derived from evidence |
| Commitments | `finops_aws.py ce-commitment` | `sql/aws-cur2-commitments.sql` | Gap |
| Telemetry | CloudWatch, Prometheus | | Cause recorded as not established |

Cost Explorer is for a first breakdown and a cross-check. CUR in Athena is for line items,
custom attribution, and reproducible SQL. Without CUR, Cost Explorer still answers service
and usage-type questions, and the report says resource-level detail isn't available.

[AWS billing versus Cost Explorer](https://docs.aws.amazon.com/cost-management/latest/userguide/differences-billing-data-cost-explorer-data.html)

## Invoices

A billing period can have several invoices: AWS, Marketplace, one-time purchases, and credit
memos. `finops_aws.py invoice` lists each one with the path to its amount. Make one figure per
invoice, and add them with a `sum` calculation. Don't take the first one. Invoices in two
currencies print a gap, because they can't be summed.

An empty list means no invoice has been issued yet, which is common early in a month. That's a
gap, not a zero.

## Prepared queries

All are in `sql/`. Pass the table as a parameter, and read the schema first with
`finops_aws.py schema`. The CUR 2.0 partition column is assumed to be `billing_period`; if the
schema says otherwise, use `--partition-column`.

| File | Parameters | Columns | Answers |
| :--- | :--- | :--- | :--- |
| `sql/aws-cur2-control-total.sql` | `table`, `period` | `billing_period,currency,billed_cost` | What do we owe? |
| `sql/aws-cur2-drivers.sql` | `table`, `period` | `billing_period,product,usage_type,currency,quantity,billed_cost` | What moved? |
| `sql/aws-cur2-effective.sql` | `table`, `period` | `billing_period,currency,effective_cost` | What did it consume? |
| `sql/aws-cur2-no-owner.sql` | `table`, `period`, `owner_tag` | `billing_period,currency,no_owner_cost` | What has no owner? |
| `sql/aws-cur2-commitments.sql` | `table`, `period` | `billing_period,total_commitment,used_commitment,unused_commitment` | Is the commitment used? |
| `sql/aws-cur2-freshness.sql` | `table`, `period` | `billing_period,max_usage_end` | How current is the data? |
| `sql/aws-cur-legacy-control-total.sql` | `table`, `year`, `month` | `billing_period,currency,billed_cost` | Legacy CUR, `--partition-column year` |
| `sql/aws-cur-legacy-drivers.sql` | `table`, `year`, `month` | `billing_period,product,usage_type,currency,quantity,billed_cost` | Legacy CUR, `--partition-column year` |

Ad hoc queries are allowed under the same guard: one read-only statement, named columns, and a
partition filter.

```bash
scripts/finops_aws.py athena <run_dir> --workgroup finops --database billing \
  --query-file sql/aws-cur2-control-total.sql --param table=cur --param period=2026-10 \
  --source "Athena, CUR 2.0" --basis billed
```

## Cost control

- **Athena:** the docs checked describe no dry run. The per-query limit is a workgroup setting
  that cancels the query, and cancelled queries are still charged. After each query the script
  reads the scanned bytes, and it refuses the next query once the budget is used up. Without a
  workgroup limit, one query can overshoot.
- **Cost Explorer:** each request costs $0.01, and the script counts every page. Hourly
  granularity isn't offered, because it has its own charge. A budget stop with a page pending
  marks the evidence incomplete, and incomplete evidence can't back a figure.

[Athena usage controls](https://docs.aws.amazon.com/athena/latest/ug/workgroups-setting-control-limits-cloudwatch.html),
[Cost Explorer pricing](https://aws.amazon.com/aws-cost-management/aws-cost-explorer/pricing/)

## Accounting traps

- **Cash and amortized views.** An upfront commitment makes a large billed month, while its
  economic cost belongs across the term. Don't compare them, and say which basis a figure is on.
- **Adding upfront purchases to amortized charges** counts the commitment twice.
  `sql/aws-cur2-effective.sql` counts upfront fees as zero, because covered usage already carries
  their share.
- **A plain sum of unblended cost isn't effective cost.** Covered usage and fees each need their
  own column. [Savings Plan fields](https://docs.aws.amazon.com/cur/latest/userguide/savingsplans-columns.html)
- **RIFee rows.** Sources disagree on the effective-cost treatment, so compare
  `sql/aws-cur2-effective.sql` with Cost Explorer's `AmortizedCost` on real data before relying on
  reservation-heavy accounts.
- **Split cost allocation rows aren't a second bill.** They divide a parent instance's cost
  between containers. [Split cost allocation](https://docs.aws.amazon.com/cur/latest/userguide/split-cost-allocation-data.html)
- **Delivery is delayed.** Exports refresh at least daily, so a quiet latest day proves nothing.
  [Export timing](https://docs.aws.amazon.com/cur/latest/userguide/dataexports-create.html)

## Commitments

Report coverage and utilization, and show the break-even math in the simplified single-rate model
with that limit stated. Real purchase advice needs hourly data, program terms, and eligibility, so
don't recommend a specific purchase.

## What to look at

Start with account, service, region, usage type, charge type, and resource. Then test the largest
contributors against telemetry and recent changes:

- **Compute:** instance hours, request sizing, autoscaling, utilization peaks, Spot suitability.
- **Storage:** growth, unattached resources, snapshots, retention, lifecycle transitions.
- **Network:** NAT, cross-zone and cross-region transfer, egress, topology changes.
- **Databases:** provisioned capacity, storage and IO, replicas, retention.
- **Kubernetes:** workload requests, node pools, idle capacity, shared services.

These are places to look, not promises that an account contains waste.
