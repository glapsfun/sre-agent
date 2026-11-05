-- Bytes billed and slot time by user, from INFORMATION_SCHEMA.JOBS. Parent SCRIPT jobs
-- are left out because their child jobs are already counted. Slot time is consumption,
-- not an amount owed, and bytes billed are informational under capacity pricing.
SELECT
  user_email,
  SUM(total_bytes_billed) AS total_bytes_billed,
  SUM(total_slot_ms) AS total_slot_ms
FROM `{{table}}`
WHERE job_type = 'QUERY'
  AND COALESCE(statement_type, '') <> 'SCRIPT'
  AND creation_time >= CAST('{{start_date}}' AS TIMESTAMP)
  AND creation_time < CAST('{{end_date}}' AS TIMESTAMP)
GROUP BY user_email
ORDER BY total_bytes_billed DESC
