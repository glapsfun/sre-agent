-- Net cost of committed use for two invoice months, from the GCP billing export. Newer
-- spend-based programs use discounted prices and fee offsets where older ones used
-- list prices and credits, so compare net cost: less credit volume is not lost savings.
-- Establish the program and migration date first, and check the fee SKU names and
-- credit types against real data.
-- invoice.month holds YYYYMM. A run's periods are YYYY-MM, so this converts both ways.
SELECT
  CONCAT(SUBSTR(invoice.month, 1, 4), '-', SUBSTR(invoice.month, 5, 2)) AS invoice_month,
  SUM(CASE WHEN sku.description LIKE 'Commitment%' THEN 0 ELSE CAST(cost AS NUMERIC) END) AS usage_cost,
  SUM(COALESCE((SELECT SUM(CAST(c.amount AS NUMERIC)) FROM UNNEST(credits) AS c WHERE c.type IN ('COMMITTED_USAGE_DISCOUNT', 'COMMITTED_USAGE_DISCOUNT_DOLLAR_BASE', 'FEE_UTILIZATION_OFFSET')), 0)) AS credits,
  SUM(CASE WHEN sku.description LIKE 'Commitment%' THEN CAST(cost AS NUMERIC) ELSE 0 END) AS fee_cost,
  SUM(CAST(cost AS NUMERIC))
    + SUM(COALESCE((SELECT SUM(CAST(c.amount AS NUMERIC)) FROM UNNEST(credits) AS c WHERE c.type IN ('COMMITTED_USAGE_DISCOUNT', 'COMMITTED_USAGE_DISCOUNT_DOLLAR_BASE', 'FEE_UTILIZATION_OFFSET')), 0)) AS net_cost
FROM `{{table}}`
WHERE invoice.month IN (REPLACE('{{first_month}}', '-', ''), REPLACE('{{second_month}}', '-', ''))
GROUP BY invoice_month
ORDER BY invoice_month
