-- Deliberately wrong. Summing unblended cost is the billed view: it counts the
-- upfront purchase and misses the effective rate of covered usage.
SELECT SUM(CAST(line_item_unblended_cost AS DECIMAL(38, 10))) AS effective_cost
FROM {{table}}
WHERE billing_period = '{{period}}'
