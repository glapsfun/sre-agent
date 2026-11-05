-- Billed cost and usage by product, usage type, and line item type, before tax, from
-- CUR 2.0 in Athena. The line item type keeps credits and fees apart from usage, or a
-- credit would read as a lower rate. Split cost allocation columns are left out on
-- purpose: they divide a parent line's cost between containers and are not a second bill.
SELECT
  billing_period,
  line_item_product_code AS product,
  line_item_usage_type AS usage_type,
  line_item_line_item_type AS line_item_type,
  line_item_currency_code AS currency,
  SUM(CAST(line_item_usage_amount AS DECIMAL(38, 10))) AS quantity,
  SUM(CAST(line_item_unblended_cost AS DECIMAL(38, 10))) AS billed_cost
FROM {{table}}
WHERE billing_period = '{{period}}'
  AND line_item_line_item_type <> 'Tax'
GROUP BY billing_period, line_item_product_code, line_item_usage_type, line_item_line_item_type, line_item_currency_code
ORDER BY billed_cost DESC, product, usage_type
