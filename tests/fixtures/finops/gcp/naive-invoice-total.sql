-- Deliberately wrong. Joining both repeated fields multiplies a row's cost and
-- credits, and the inner joins drop rows with no credits or labels. The test
-- expects these wrong totals, which proves the fixture catches the trap.
SELECT
  SUM(CAST(cost AS DECIMAL(38, 9))) AS cost,
  SUM(CAST(c.amount AS DECIMAL(38, 9))) AS credits
FROM {{table}}
CROSS JOIN UNNEST(credits) AS t1(c)
CROSS JOIN UNNEST(labels) AS t2(l)
WHERE invoice.month = '{{invoice_month}}'
