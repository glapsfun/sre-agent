-- Cost plus credits by when usage happened, from the GCP billing export. This answers
-- what a workload consumed. It won't match an invoice, because late usage lands on a
-- later invoice month.
SELECT
  FORMAT_TIMESTAMP('%Y-%m', usage_start_time) AS usage_month,
  currency,
  SUM(CAST(cost AS NUMERIC) + COALESCE((SELECT SUM(CAST(c.amount AS NUMERIC)) FROM UNNEST(credits) AS c), 0)) AS effective_cost
FROM `{{table}}`
WHERE usage_start_time >= CAST('{{usage_start}}' AS TIMESTAMP)
  AND usage_start_time < CAST('{{usage_end}}' AS TIMESTAMP)
  AND cost_type <> 'tax'
GROUP BY usage_month, currency
ORDER BY usage_month, currency
