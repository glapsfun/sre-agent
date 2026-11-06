-- Savings Plan commitment and use for one billing period, from CUR 2.0 in Athena.
-- The fallback for when Cost Explorer's utilization calls aren't available.
SELECT
  billing_period,
  SUM(CAST(savings_plan_total_commitment_to_date AS DECIMAL(38, 10))) AS total_commitment,
  SUM(CAST(savings_plan_used_commitment AS DECIMAL(38, 10))) AS used_commitment,
  SUM(CAST(savings_plan_total_commitment_to_date - savings_plan_used_commitment AS DECIMAL(38, 10))) AS unused_commitment
FROM {{table}}
WHERE billing_period = '{{period}}'
  AND line_item_line_item_type = 'SavingsPlanRecurringFee'
GROUP BY billing_period
