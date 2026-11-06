-- Billed total for one billing period, from CUR 2.0 in Athena. This is the number
-- reconciled to the invoice, so tax is its own column: invoice figures are before tax.
-- Upfront purchases count here in full, which is correct for what is owed and wrong
-- for what a workload consumed.
SELECT
  billing_period,
  line_item_currency_code AS currency,
  SUM(CASE WHEN line_item_line_item_type = 'Tax' THEN 0 ELSE CAST(line_item_unblended_cost AS DECIMAL(38, 10)) END) AS billed_cost,
  SUM(CASE WHEN line_item_line_item_type = 'Tax' THEN CAST(line_item_unblended_cost AS DECIMAL(38, 10)) ELSE 0 END) AS tax
FROM {{table}}
WHERE billing_period = '{{period}}'
GROUP BY billing_period, line_item_currency_code
ORDER BY currency
