-- Invoice total for one invoice month, from the GCP billing export.
-- Credits are aggregated inside each billing row before grouping, because a row
-- can hold several credits and joining them would multiply its cost.
-- Filter on invoice.month, not usage time: late usage lands on a later invoice.
SELECT
  invoice.month AS invoice_month,
  currency,
  SUM(CAST(cost AS NUMERIC)) AS cost,
  SUM(COALESCE((SELECT SUM(CAST(c.amount AS NUMERIC)) FROM UNNEST(credits) AS c), 0)) AS credits,
  SUM(CAST(cost AS NUMERIC))
    + SUM(COALESCE((SELECT SUM(CAST(c.amount AS NUMERIC)) FROM UNNEST(credits) AS c), 0)) AS net_cost
FROM `{{table}}`
WHERE invoice.month = '{{invoice_month}}'
GROUP BY invoice_month, currency
ORDER BY currency
