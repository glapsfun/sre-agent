-- Cost, credits, and usage by service, SKU, and location for one invoice month, from
-- the GCP billing export. Credits are aggregated inside each row before grouping,
-- because joining them would multiply the row's cost.
-- invoice.month holds YYYYMM. A run's periods are YYYY-MM, so this converts both ways.
SELECT
  CONCAT(SUBSTR(invoice.month, 1, 4), '-', SUBSTR(invoice.month, 5, 2)) AS invoice_month,
  service.description AS service,
  sku.description AS sku,
  location.location AS location,
  currency,
  SUM(CAST(usage.amount_in_pricing_units AS NUMERIC)) AS usage_amount,
  usage.pricing_unit AS usage_unit,
  SUM(CAST(cost AS NUMERIC)) AS cost,
  SUM(COALESCE((SELECT SUM(CAST(c.amount AS NUMERIC)) FROM UNNEST(credits) AS c), 0)) AS credits,
  SUM(CAST(cost AS NUMERIC))
    + SUM(COALESCE((SELECT SUM(CAST(c.amount AS NUMERIC)) FROM UNNEST(credits) AS c), 0)) AS net_cost
FROM `{{table}}`
WHERE invoice.month = REPLACE('{{invoice_month}}', '-', '')
  AND cost_type <> 'tax'
GROUP BY invoice_month, service.description, sku.description, location.location, currency, usage.pricing_unit
ORDER BY net_cost DESC
