-- Billed total for one billing period, from a legacy CUR in Athena. Legacy tables are
-- partitioned by year and month, with month not zero-padded. Tax is its own column,
-- because invoice figures are before tax.
SELECT
  CONCAT(year, '-', LPAD(month, 2, '0')) AS billing_period,
  line_item_currency_code AS currency,
  SUM(CASE WHEN line_item_line_item_type = 'Tax' THEN 0 ELSE CAST(line_item_unblended_cost AS DECIMAL(38, 10)) END) AS billed_cost,
  SUM(CASE WHEN line_item_line_item_type = 'Tax' THEN CAST(line_item_unblended_cost AS DECIMAL(38, 10)) ELSE 0 END) AS tax
FROM {{table}}
WHERE year = '{{year}}' AND month = '{{month}}'
GROUP BY year, month, line_item_currency_code
ORDER BY currency
