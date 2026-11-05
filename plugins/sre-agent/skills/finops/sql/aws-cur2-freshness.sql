-- Latest usage in a billing period, from CUR 2.0 in Athena. Exports arrive late, so
-- a quiet last day is not evidence of lower spend. This sets the data-through date.
SELECT
  billing_period,
  MAX(line_item_usage_end_date) AS max_usage_end
FROM {{table}}
WHERE billing_period = '{{period}}'
GROUP BY billing_period
