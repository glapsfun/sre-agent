-- Billed cost with no value for the owner tag, from CUR 2.0 in Athena. element_at
-- returns NULL for a missing key, where a plain subscript raises an error in Athena.
SELECT
  billing_period,
  line_item_currency_code AS currency,
  SUM(CAST(line_item_unblended_cost AS DECIMAL(38, 10))) AS no_owner_cost
FROM {{table}}
WHERE billing_period = '{{period}}'
  AND COALESCE(element_at(resource_tags, '{{owner_tag}}'), '') = ''
GROUP BY billing_period, line_item_currency_code
ORDER BY currency
