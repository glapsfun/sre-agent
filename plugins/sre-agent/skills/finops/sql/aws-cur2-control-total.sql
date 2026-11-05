-- Billed total for one billing period, from CUR 2.0 in Athena. This is the number
-- reconciled to the invoice. Upfront purchases count here in full, which is correct
-- for what is owed and wrong for what a workload consumed.
SELECT
  billing_period,
  line_item_currency_code AS currency,
  SUM(CAST(line_item_unblended_cost AS DECIMAL(38, 10))) AS billed_cost
FROM {{table}}
WHERE billing_period = '{{period}}'
GROUP BY billing_period, line_item_currency_code
ORDER BY currency
