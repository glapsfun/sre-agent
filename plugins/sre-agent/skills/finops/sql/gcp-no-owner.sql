-- Cost plus credits with no value for the owner label, for one invoice month, from the
-- GCP billing export. A subquery rather than a join, so labels can't multiply cost.
SELECT
  invoice.month AS invoice_month,
  currency,
  SUM(CAST(cost AS NUMERIC) + COALESCE((SELECT SUM(CAST(c.amount AS NUMERIC)) FROM UNNEST(credits) AS c), 0)) AS no_owner_cost
FROM `{{table}}`
WHERE invoice.month = '{{invoice_month}}'
  AND cost_type <> 'tax'
  AND NOT EXISTS (SELECT 1 FROM UNNEST(labels) AS l WHERE l.key = '{{owner_label}}' AND l.value <> '')
GROUP BY invoice_month, currency
ORDER BY currency
