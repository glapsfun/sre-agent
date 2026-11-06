-- Latest usage in a billing period, from CUR 2.0 in Athena. Exports arrive late, so
-- a quiet last day is not evidence of lower spend. This sets the data-through date.
-- Only usage lines count: a monthly fee line can carry the month's end date, which
-- would make a partial month look complete.
SELECT
  billing_period,
  MAX(line_item_usage_end_date) AS max_usage_end
FROM {{table}}
WHERE billing_period = '{{period}}'
  AND line_item_line_item_type IN ('Usage', 'DiscountedUsage', 'SavingsPlanCoveredUsage')
GROUP BY billing_period
