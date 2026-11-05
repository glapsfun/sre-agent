-- Billed total for one billing period, from a legacy CUR in Athena. Legacy tables are
-- partitioned by year and month, with month not zero-padded.
SELECT
  CONCAT(year, '-', LPAD(month, 2, '0')) AS billing_period,
  line_item_currency_code AS currency,
  SUM(CAST(line_item_unblended_cost AS DECIMAL(38, 10))) AS billed_cost
FROM {{table}}
WHERE year = '{{year}}' AND month = '{{month}}'
GROUP BY year, month, line_item_currency_code
ORDER BY currency
