-- Deliberately wrong for an invoice. Filtering on usage time misses late usage that
-- landed on this invoice, so the total can't reconcile.
SELECT SUM(CAST(cost AS DECIMAL(38, 9)) + COALESCE((SELECT SUM(CAST(c.amount AS DECIMAL(38, 9))) FROM UNNEST(credits) AS c), 0)) AS net_cost
FROM {{table}}
WHERE strftime(usage_start_time, '%Y-%m') = '{{invoice_month}}'
