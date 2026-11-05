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
| `invoicing list-invoice-summaries` returns invoice totals by billing period | [CLI reference](https://docs.aws.amazon.com/cli/latest/reference/invoicing/list-invoice-summaries.html) |
| `cost-optimization-hub list-recommendations` returns `estimatedMonthlySavings` and can dedupe by resource | [CLI reference](https://docs.aws.amazon.com/cli/latest/reference/cost-optimization-hub/list-recommendations.html) |
| Athena's per-query limit is a workgroup setting, 10 MB minimum; it cancels the query, and cancelled queries are charged | [Athena docs](https://docs.aws.amazon.com/athena/latest/ug/workgroups-setting-control-limits-cloudwatch.html) |
| `bq --dry_run` reports bytes without billing; over `--maximum_bytes_billed`, a query fails without charge | [BigQuery docs](https://docs.cloud.google.com/bigquery/docs/best-practices-costs) |
| GCP export table names | [GCP docs](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery-tables) |
| FOCUS dataset naming | [GCP docs](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery-focus-setup?hl=en) |
| OpenCost `/allocation`, port 9003, parameters | [OpenCost docs](https://opencost.io/docs/integrations/api/) |
| Kubecost `/model/allocation`, port 9090, and `/allocation` on the cost-model container, for 1.x and 2.x (from a search summary, pages not read in full) | [IBM Kubecost 2.x](https://www.ibm.com/docs/en/SSW0JQG_2.x/apis/monitoring-apis/api-allocation.html), [1.x](https://www.ibm.com/docs/en/SSW0JQG_1.x/apis/apis-overview/api-allocation.html) |
| `gcloud recommender recommendations list` needs a location, a recommender, and one scope | [gcloud reference](https://docs.cloud.google.com/sdk/gcloud/reference/recommender/recommendations/list) |
| The Cloud Billing REST reference lists no method returning incurred cost | [REST reference](https://docs.cloud.google.com/billing/docs/reference/rest) |

## Not confirmed

- That every discovery call is free.
- That Athena has no dry run or scan estimate. The page checked only doesn't mention one.
- The `invoicing` service's endpoint region and required permissions.
- That no Cloud Billing response schema exposes incurred cost. This rests on the REST
  reference's summary page and community replies.
- Kubecost's full parameter list, and routes for versions newer than 2.x.
- The Cost Explorer utilization and coverage command names.
