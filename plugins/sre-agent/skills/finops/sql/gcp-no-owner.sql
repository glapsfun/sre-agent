-- Cost plus credits with no value for the owner label, for one invoice month, from the
-- GCP billing export. A subquery rather than a join, so labels can't multiply cost.
-- invoice.month holds YYYYMM. A run's periods are YYYY-MM, so this converts both ways.
SELECT
  CONCAT(SUBSTR(invoice.month, 1, 4), '-', SUBSTR(invoice.month, 5, 2)) AS invoice_month,
  currency,
  SUM(CAST(cost AS NUMERIC) + COALESCE((SELECT SUM(CAST(c.amount AS NUMERIC)) FROM UNNEST(credits) AS c), 0)) AS no_owner_cost
FROM `{{table}}`
WHERE invoice.month = REPLACE('{{invoice_month}}', '-', '')
  AND cost_type <> 'tax'
  AND NOT EXISTS (SELECT 1 FROM UNNEST(labels) AS l WHERE l.key = '{{owner_label}}' AND l.value <> '')
GROUP BY invoice_month, currency
ORDER BY currency
