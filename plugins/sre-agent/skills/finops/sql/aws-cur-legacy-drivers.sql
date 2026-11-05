-- Billed cost and usage by product and usage type, from a legacy CUR in Athena.
SELECT
  CONCAT(year, '-', LPAD(month, 2, '0')) AS billing_period,
  line_item_product_code AS product,
  line_item_usage_type AS usage_type,
  line_item_currency_code AS currency,
  SUM(CAST(line_item_usage_amount AS DECIMAL(38, 10))) AS quantity,
  SUM(CAST(line_item_unblended_cost AS DECIMAL(38, 10))) AS billed_cost
FROM {{table}}
WHERE year = '{{year}}' AND month = '{{month}}'
GROUP BY year, month, line_item_product_code, line_item_usage_type, line_item_currency_code
ORDER BY billed_cost DESC, product, usage_type
