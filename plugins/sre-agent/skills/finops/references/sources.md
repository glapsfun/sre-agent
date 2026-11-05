# Sources

Primary sources behind this skill, with the date each fact was checked. Provider
documentation and features change, so recheck anything a decision depends on.

## Confirmed

Checked 2026-11-05 unless noted.

| Fact | Source |
| :--- | :--- |
| Cost Explorer costs $0.01 per request; hourly granularity over 14 days is charged separately | [AWS pricing](https://aws.amazon.com/aws-cost-management/aws-cost-explorer/pricing/) |
| Cost Explorer, Data Exports, and Cost Optimization Hub have a `us-east-1` endpoint only | [AWS endpoints](https://docs.aws.amazon.com/general/latest/gr/billing.html) |
| `bcm-data-exports list-exports` returns name and status, including `LastRefreshedAt` | [CLI reference](https://docs.aws.amazon.com/cli/latest/reference/bcm-data-exports/list-exports.html) |
| `bcm-data-exports get-export` returns `Export.DataQuery.QueryStatement` and `TableConfigurations`; standard tables include CUR 2.0 and FOCUS 1.0 and 1.2 | [CLI reference](https://docs.aws.amazon.com/cli/latest/reference/bcm-data-exports/get-export.html), [Data Exports](https://docs.aws.amazon.com/cur/latest/userguide/what-is-data-exports.html) |
| Cost Optimization Hub enrollment is read with `list-enrollment-statuses` | [CLI reference](https://docs.aws.amazon.com/cli/latest/reference/cost-optimization-hub/index.html) |
| Cost Explorer has `get-savings-plans-utilization`, `get-savings-plans-coverage`, `get-reservation-utilization`, and `get-reservation-coverage` | [CLI reference](https://docs.aws.amazon.com/cli/latest/reference/ce/index.html) |
| `invoicing list-invoice-summaries` returns invoice totals by billing period | [CLI reference](https://docs.aws.amazon.com/cli/latest/reference/invoicing/list-invoice-summaries.html) |
| `cost-optimization-hub list-recommendations` returns `estimatedMonthlySavings` and can dedupe by resource | [CLI reference](https://docs.aws.amazon.com/cli/latest/reference/cost-optimization-hub/list-recommendations.html) |
| Athena's per-query limit is a workgroup setting, 10 MB minimum; it cancels the query, and cancelled queries are charged | [Athena docs](https://docs.aws.amazon.com/athena/latest/ug/workgroups-setting-control-limits-cloudwatch.html) |
| `bq --dry_run` reports bytes without billing; over `--maximum_bytes_billed`, a query fails without charge | [BigQuery docs](https://docs.cloud.google.com/bigquery/docs/best-practices-costs) |
| `bq query` returns 100 rows unless `--max_rows` is set, and `bq show` needs `--location` for jobs | [bq reference](https://docs.cloud.google.com/bigquery/docs/reference/bq-cli-reference) |
| Job fields `statistics.query.totalBytesBilled`, `statistics.query.totalBytesProcessed`, and `statistics.reservation_id` | [Job resource](https://docs.cloud.google.com/bigquery/docs/reference/rest/v2/Job) |
| `INFORMATION_SCHEMA.JOBS` needs a region qualifier, and summing costs should exclude `SCRIPT` statements | [JOBS view](https://docs.cloud.google.com/bigquery/docs/information-schema-jobs) |
| Detailed export columns, with `cost` and `credits.amount` as floats, cast to NUMERIC before summing | [Detailed usage schema](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery-tables/detailed-usage) |
| Cloud Monitoring `projects.timeSeries.list` takes `filter` and `interval` | [Monitoring API](https://docs.cloud.google.com/monitoring/api/ref_v3/rest/v3/projects.timeSeries/list) |
| GCP export table names | [GCP docs](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery-tables) |
| FOCUS dataset naming | [GCP docs](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery-focus-setup?hl=en) |
| OpenCost `/allocation`, port 9003, parameters | [OpenCost docs](https://opencost.io/docs/integrations/api/) |
| Kubecost `GET /model/allocation`, port 9090, parameters `window`, `aggregate`, `accumulate`, `idle` (default true), `shareIdle` (default false), `splitIdle`, `external`; response has `code`, `data`, `totalCost`. The 1.x page shows the same route | [IBM Kubecost 2.x](https://www.ibm.com/docs/en/SSW0JQG_2.x/apis/monitoring-apis/api-allocation.html), [1.x](https://www.ibm.com/docs/en/SSW0JQG_1.x/apis/apis-overview/api-allocation.html) |
| `gcloud recommender recommendations list` needs a location, a recommender, and one scope | [gcloud reference](https://docs.cloud.google.com/sdk/gcloud/reference/recommender/recommendations/list) |
| The Cloud Billing REST reference lists no method returning incurred cost, and its API discovery document shows no schema holding one | [REST reference](https://docs.cloud.google.com/billing/docs/reference/rest), [discovery document](https://cloudbilling.googleapis.com/$discovery/rest?version=v1) |

## Background

Checked 2026-11-05. These informed the workflow and the accounting rules.

- [FinOps phases](https://www.finops.org/framework/phases/), [framework](https://www.finops.org/framework/), and [unit economics](https://www.finops.org/framework/capabilities/unit-economics/)
- [FOCUS cost comparison](https://focus.finops.org/docs/specification/v1-4/features/cost-comparison/). Report the FOCUS version actually in the data, and don't normalize across providers
- [AWS billing versus Cost Explorer](https://docs.aws.amazon.com/cost-management/latest/userguide/differences-billing-data-cost-explorer-data.html), [CUR 2.0 dictionary](https://docs.aws.amazon.com/cur/latest/userguide/table-dictionary-cur2.html), [Savings Plan fields](https://docs.aws.amazon.com/cur/latest/userguide/savingsplans-columns.html), [split cost allocation](https://docs.aws.amazon.com/cur/latest/userguide/split-cost-allocation-data.html), [Cost Optimization Hub](https://docs.aws.amazon.com/cost-management/latest/userguide/cost-optimization-hub.html)
- [GCP export overview](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery), [GCP query examples](https://docs.cloud.google.com/billing/docs/how-to/bq-examples), [spend-based CUD models](https://docs.cloud.google.com/docs/cuds-multiprice), [GKE cost allocation](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/cost-allocations)
- [OpenCost specification](https://opencost.io/docs/specification/)

No reported customer saving from any source is used as a promised outcome.

## Not confirmed

- That every discovery call is free. AWS documents a per-request charge only for Cost Explorer, which discovery never calls. The BigQuery pricing page could not be read when checked, so free dataset and table listing rests on general knowledge.
- That Athena has no dry run or scan estimate. The page checked only doesn't mention one.
- The `invoicing` service's endpoint region and required permissions. AWS's endpoints page doesn't list it, so the script pins `us-east-1` like other billing services, and a wrong region shows up as a gap.
- The effective-cost treatment of `RIFee` rows. Community sources disagree, and one reports this formula not matching Cost Explorer's amortized view. Compare `aws-cur2-effective.sql` with Cost Explorer `AmortizedCost` on real data.
- That CUR 2.0 in Athena is partitioned on `billing_period`. The docs describe a Glue crawler loading partitions but don't name the column. Run `finops_aws.py schema` first, and use `--partition-column` if it differs.
- The output format of `bq query --dry_run --format=json`. The docs don't describe it, so the script reads the job JSON and also the plain-text form.
- GCP committed use fee SKU names and credit types. `gcp-cud.sql` looks for SKUs starting `Commitment` and three credit types, which need checking against real data. It reads only the billing export, not a separate CUD metadata table.
- Kubecost routes for versions newer than 2.x. The 2.x page links to a 3.x migration guide but doesn't document 3.x APIs, so the script refuses 3.x.
