-- Deliberately wrong. Split cost columns divide a parent line's cost; adding them
-- to it treats the allocation as a second bill.
SELECT SUM(
  CAST(line_item_unblended_cost AS DECIMAL(38, 10))
  + COALESCE(CAST(split_line_item_split_cost AS DECIMAL(38, 10)), 0)
  + COALESCE(CAST(split_line_item_unused_cost AS DECIMAL(38, 10)), 0)
) AS billed_cost
FROM {{table}}
WHERE billing_period = '{{period}}'
