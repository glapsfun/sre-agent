-- Billed cost and usage by product, usage type, and line item type, before tax, from a
-- legacy CUR in Athena. The line item type keeps credits and fees apart from usage.
SELECT
  CONCAT(year, '-', LPAD(month, 2, '0')) AS billing_period,
  line_item_product_code AS product,
  line_item_usage_type AS usage_type,
  line_item_line_item_type AS line_item_type,
  line_item_currency_code AS currency,
  SUM(CAST(line_item_usage_amount AS DECIMAL(38, 10))) AS quantity,
  SUM(CAST(line_item_unblended_cost AS DECIMAL(38, 10))) AS billed_cost
FROM {{table}}
WHERE year = '{{year}}' AND month = '{{month}}'
  AND line_item_line_item_type <> 'Tax'
GROUP BY year, month, line_item_product_code, line_item_usage_type, line_item_line_item_type, line_item_currency_code
ORDER BY billed_cost DESC, product, usage_type
