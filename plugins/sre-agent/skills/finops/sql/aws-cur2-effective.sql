-- Effective (amortized) cost for one billing period, from CUR 2.0 in Athena.
-- No single column gives this, so each line item type uses its own. Upfront fees
-- count as zero here because covered usage already carries their amortized share.
-- Sources disagree on RIFee, so compare this with Cost Explorer's AmortizedCost on
-- real data before relying on reservation-heavy accounts.
SELECT
  billing_period,
  line_item_currency_code AS currency,
  SUM(CAST(
    CASE
      WHEN line_item_line_item_type = 'SavingsPlanCoveredUsage' THEN savings_plan_savings_plan_effective_cost
      WHEN line_item_line_item_type = 'SavingsPlanRecurringFee' THEN savings_plan_total_commitment_to_date - savings_plan_used_commitment
      WHEN line_item_line_item_type IN ('SavingsPlanNegation', 'SavingsPlanUpfrontFee') THEN 0
      WHEN line_item_line_item_type = 'DiscountedUsage' THEN reservation_effective_cost
      WHEN line_item_line_item_type = 'RIFee' THEN reservation_unused_amortized_upfront_fee_for_billing_period + reservation_unused_recurring_fee
      WHEN line_item_line_item_type = 'Fee' AND reservation_reservation_a_r_n <> '' THEN 0
      ELSE line_item_unblended_cost
    END AS DECIMAL(38, 10)
  )) AS effective_cost
FROM {{table}}
WHERE billing_period = '{{period}}'
  AND line_item_line_item_type <> 'Tax'
GROUP BY billing_period, line_item_currency_code
ORDER BY currency
