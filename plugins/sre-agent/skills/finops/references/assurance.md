# Assurance

The promise: every number is real and traceable. This is how a run keeps it, and where it stops.

## Report status

Each report opens with one status, the first in this list that applies. It always describes the billed
control total. It's computed by the validator, never written by hand.

| Status | Meaning |
| :--- | :--- |
| Not reconciled | A residual is outside tolerance, or a closed whole-bill period has no invoice figure and no second source. The reason is stated |
| Open period | A period in the report hasn't closed. Figures are provisional |
| Partial scope | The scope is less than a whole bill |
| Cross-checked only | Matches a second billing view, but at least one period has no invoice figure |
| Reconciled to invoice | Whole bill, closed periods, each within tolerance of its invoice figure |

Tolerance defaults to 0.1% of the invoice figure, and the residual is always printed. A failed
reconciliation doesn't stop the run: the report opens with "Not reconciled".

Effective figures are labeled and aren't compared to the invoice. When a report has both, the
reconciliation table shows the difference between the billed and effective totals as a calculation.

## Figures and tags

| Kind | Meaning | Tag |
| :--- | :--- | :--- |
| Measured | Read directly from a billing source's saved result, or telemetry | `E<n>` |
| Provided | Typed by a person. Records what it is, who gave it, and when | `P<n>` |
| Calculated | Script output using only measured or provided inputs | `C<n>` |
| Estimated | Includes an assumption, a provider's own estimate, or a cost-tool allocation. Assumptions are printed | `C<n> est` or `E<n> est` |

## How the numbers are proved

Query scripts, not the model, write evidence entries. Each records the source, exact query or command,
profile or account, job ID, bytes or requests used, time, and the saved raw result with its SHA-256.

A measured figure points to its place in the raw result. Calculation inputs are references too. The model
writes findings, assumptions, and references, so it can't introduce a measured number that isn't in a raw
result. The report renders values from those references, so it can't print one either.

## Validation failures

| Code | Means |
| :--- | :--- |
| `HASH_MISMATCH`, `RAW_MISSING` | A raw result changed or is gone |
| `FIGURE_MISMATCH` | A figure differs from the value at its location |
| `LOCATOR_FAILED` | The location finds no value, or more than one |
| `EVIDENCE_INCOMPLETE` | The figure rests on partial results |
| `FIGURE_KIND` | The evidence can't back that kind of figure, such as measured on a cost-tool allocation |
| `UNRESOLVED_REF` | A reference or tag has no entry |
| `UNTAGGED_MONEY` | Free text holds a money figure. Use a reference |
| `CALC_MISMATCH` | Recomputing gives a different result, or it can't be recomputed |
| `CALC_UNKNOWN` | No such calculation |
| `CALC_KIND` | Marked calculated but uses an assumption or estimate, or an assumption is unstated |
| `CURRENCY_MIX` | One calculation mixes currencies |
| `RECON_RESIDUAL` | A residual doesn't match its period's invoice and total |
| `DRIVER_SUM` | Drivers don't sum to the total change. End with an `Other` row |
| `LEDGER_SUM`, `LEDGER_KIND`, `POOL_SUM` | The Kubernetes ledger double-counts or loses cost |
| `OVERLAP` | Two opportunities on one resource are summed. Use `overlap_savings` |
| `CAUSE_NO_EVIDENCE` | A stated cause cites nothing |
| `PEAK_MISSING` | Rightsizing with no telemetry peak, and not marked `needs_investigation` |
| `INVESTIGATE_IN_TOTAL` | An item that needs investigation counts toward the savings total |
| `PAST_TOO_EARLY` | A saving is measured before a complete billing period has passed |
| `PAST_RESULT_CALC` | A measured saving isn't from `realized_savings` |
| `STATUS_CLAIM` | A claimed status or closed period doesn't hold |
| `MISSING_FIELD` | The data-through date isn't set |
| `RECORD_INVALID` | The record can't be read |

Fix a failure, or tell the user what can't be proved. A report that failed validation is never shown as
passing.

## What this doesn't prove

Say this plainly when it matters:

- **That a query asked the right question.** Reconciliation and the SQL tests address that, but only
  reconciliation runs on real data.
- **That a cited cause is the true cause.** The validator checks that evidence is cited, not that it
  supports the claim.
- **Figures from outside the bill.** A provided figure is only as good as whoever typed it.
- **Numbers in free text without a currency.** A bare `3600` in a sentence isn't caught, so keep amounts in
  references.
- **Deliberate tampering.** The hashes guard against mistakes, not someone editing files on purpose.

## Period rules

Compare complete, equivalent periods. Exclude days that are incomplete because of export delay, and state
the data-through date. A quiet latest day is not evidence of lower spend.

A saving counts as measured only after a complete billing period since the change. The baseline is the last
complete month before it, and the comparison is the first month starting after it.
