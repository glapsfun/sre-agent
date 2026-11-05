# The run record and report

Each run has a directory:

```text
docs/finops/<YYYY-MM-DD>-<provider>-<scope>-<period>/
  report.md
  run.json
  evidence/
```

`run.json` is the source of truth. Scripts write `scope`, `budget`, `evidence`, and `provided`. You write
everything else with `scripts/finops_record.py merge <run_dir> <fragment.json>`, which refuses those four sections.
Merging adds or replaces. To take something back, use `scripts/finops_record.py remove <run_dir> <section> [key]`,
where the key is a figure or entry ID, a period, or a gap's text.

## References

Sections hold references, never numbers:

- a figure ID, such as `oct_query_total`;
- a calculation output, such as `C5.change`.

## Sections you write

**`figures`**, keyed by ID. A CSV result is located by row and column. A JSON result is located by path.

```json
{
  "figures": {
    "oct_query_total": {
      "value": "56938.72", "kind": "measured", "currency": "USD", "basis": "billed", "evidence": "E3",
      "locator": {"type": "csv", "where": {"billing_period": "2026-10"}, "column": "billed_cost"}
    },
    "oct_invoice": {
      "value": "56940.00", "kind": "measured", "currency": "USD", "basis": "billed", "evidence": "E1",
      "locator": {"type": "json", "path": ["InvoiceSummaries", 0, "BaseCurrencyAmount", "TotalAmountBeforeTax"]}
    },
    "gcp_invoice": {"value": "355.30", "kind": "provided", "currency": "USD", "basis": "billed", "provided": "P1"},
    "oct_nat_gb": {
      "value": "200000", "kind": "measured", "currency": null, "basis": "billed", "evidence": "E3",
      "locator": {"type": "csv", "where": {"billing_period": "2026-10", "usage_type": "NatGateway-Bytes"}, "column": "quantity"}
    }
  }
}
```

**`calculations`**: see `calculations.md`.

**`reconciliation`**, one entry per period, for both the period and the comparison period. `closed`, `invoice`,
`query_total`, and `residual` drive the status. The others are optional rows. `assurance.md` says what each may hold.

```json
{
  "reconciliation": [{
    "period": "2026-10", "closed": true,
    "invoice": "oct_invoice", "second_view": null,
    "query_total": "oct_query_total", "residual": "C1.residual",
    "no_owner": "oct_no_owner", "no_owner_share": "C3.pct",
    "effective_total": null, "billed_effective_difference": null,
    "whole_bill_total": null, "scope_share": null
  }]
}
```

Total change is a `change` calculation over the two periods' `query_total` figures, not invoices, so drivers can sum to
it exactly. End drivers with an `Other` row, from a `residual` of the total change against the drivers' `sum`.

**`changes`**:

```json
{
  "changes": {
    "total": "C5.change", "pct": "C5.pct",
    "drivers": [
      {"name": "NAT data", "quantity": "C6.quantity", "rate": "C6.rate", "change": "C6.change",
       "cause": {"text": "orders-api v2.14, Oct 9", "evidence": ["E4", "E5"]}},
      {"name": "Other", "quantity": null, "rate": null, "change": "C10.residual", "cause": null}
    ]
  }
}
```

A driver with `cause: null` prints "Not established".

**`opportunities`** and **`savings_total`**. Keep IDs stable across runs: `finops_compare.py carry` prints the next free
one, and a still-open opportunity has to be listed again to stay tracked. `type` is `rightsizing` or `other`, and `resource`
is required. A saving is estimated or provided, never measured. Summing two opportunities on one resource needs an
`overlap_savings` calculation in the total instead.

```json
{
  "opportunities": [{
    "id": "O-22", "action": "Resize search node pool", "owner": "search", "resource": "search-pool",
    "type": "rightsizing", "monthly": "e7_search", "assumes": ["Provider estimate"],
    "reliability": {"text": "Peak 41% over 30d", "evidence": ["E6"]}, "peak_evidence": "E6",
    "state": "identified", "needs_investigation": false, "overlap_calc": null
  }],
  "savings_total": "C14.total"
}
```

**`past_savings`** comes from `finops_compare.py mark`. Once a period can be measured, add a `realized_savings` calculation
with no assumptions and set `result` and `status`. The status must match the result: `Landed`, `Landed, below estimate`, or
`Did not land`. With no result it's `too early to measure`, `not yet measured`, `approved, not implemented`, `rejected`, or
`cannot measure`. `avoidance` is optional, for growth-related cost avoidance, and stays separate from measured savings.

**`gaps`**: plain strings, or `{"text": "...", "ref": "C13.change"}` for one with an amount. Scripts add theirs automatically.

**`kubernetes_ledger`**: see `kubernetes-cost.md`.

Free text can't hold a money figure. Use a reference.

## Layout

Proof first: check the numbers hold, then read what they mean. All figures are synthetic.

```markdown
# FinOps report: AWS 123456789012, October 2026

Question: why did spend rise, and what can we do about it?
Status: Reconciled to invoice (tolerance 0.1%)
Billed basis, USD. Data through 2026-11-03. Compared with September 2026.
Last report: docs/finops/2026-10-05-aws-123456789012-2026-09/report.md
Query use: Athena 2.1 GB of 5 GB approved, Cost Explorer 6 of 20 requests.

## Reconciliation

| Item | October | September | Source |
| :--- | ---: | ---: | :--- |
| Invoice before tax | $56,940.00 | $48,210.00 | E1, E2 |
| Query total | $56,938.72 | $48,209.40 | E3 |
| Residual | $1.28 | $0.60 | C1, C2 |
| No owner tag | $3,210.00 (5.6%) | $2,890.00 (6.0%) | E3, C3, C4 |

## Gaps

## Evidence

## Calculations

## What changed: +$8,729.32 (18.1%) [C5]

| Driver | Quantity | Rate | Change | Cause | Source |
| :--- | ---: | ---: | ---: | :--- | :--- |
| NAT data | +$4,500.00 | $0.00 | +$4,500.00 | orders-api v2.14, Oct 9 | C6, E4, E5 |
| Other | | | -$0.68 | | C10 |

## Did past savings land?

## What to do: $6,140.00/mo estimated [C14 est]

| ID | Action | Owner | Est./mo | Assumes | Reliability check | Source |
| :--- | :--- | :--- | ---: | :--- | :--- | :--- |
| O-22 | Resize search node pool | search | $1,900.00 | Provider estimate | Peak 41% over 30d, E6 | E7 est |
```
