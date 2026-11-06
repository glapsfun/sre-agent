-- Invoice total for one invoice month, from the GCP billing export. Tax is its own
-- column, because the figure reconciled is before tax. Credits are aggregated inside
-- each billing row before grouping, because a row can hold several credits and joining
-- them would multiply its cost. Filter on invoice.month, not usage time: late usage
-- lands on a later invoice.
-- invoice.month holds YYYYMM. A run's periods are YYYY-MM, so this converts both ways.
SELECT
  CONCAT(SUBSTR(invoice.month, 1, 4), '-', SUBSTR(invoice.month, 5, 2)) AS invoice_month,
  currency,
  SUM(CASE WHEN cost_type = 'tax' THEN 0 ELSE CAST(cost AS NUMERIC) END) AS cost,
  SUM(COALESCE((SELECT SUM(CAST(c.amount AS NUMERIC)) FROM UNNEST(credits) AS c), 0)) AS credits,
  SUM(CASE WHEN cost_type = 'tax' THEN 0 ELSE CAST(cost AS NUMERIC) END)
    + SUM(COALESCE((SELECT SUM(CAST(c.amount AS NUMERIC)) FROM UNNEST(credits) AS c), 0)) AS net_cost,
  SUM(CASE WHEN cost_type = 'tax' THEN CAST(cost AS NUMERIC) ELSE 0 END) AS tax
FROM `{{table}}`
WHERE invoice.month = REPLACE('{{invoice_month}}', '-', '')
GROUP BY invoice_month, currency
ORDER BY currency
