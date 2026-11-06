-- Latest usage and export time for one invoice month, from the GCP billing export.
-- Exports arrive late, so a quiet last day is not evidence of lower spend. This sets
-- the data-through date.
-- invoice.month holds YYYYMM. A run's periods are YYYY-MM, so this converts both ways.
SELECT
  MAX(usage_end_time) AS max_usage_end,
  MAX(export_time) AS max_export_time
FROM `{{table}}`
WHERE invoice.month = REPLACE('{{invoice_month}}', '-', '')
