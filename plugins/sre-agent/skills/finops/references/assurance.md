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

Tolerance defaults to 0.1% of the invoice figure and is set with `--tolerance-pct`. The residual is
always printed. A failed reconciliation doesn't stop the run: the report opens with "Not reconciled".

A whole-bill report needs an entry for both the period and the comparison period, and each figure has to
be what its row claims:

- **Invoice:** provided by a person, or read from that period's invoice evidence. A sum must count every
  invoice in the evidence, and evidence holding two currencies can't reconcile.
- **Query total:** measured, from a query result for that period, with evidence recorded as billed basis.
- **Second view:** the same, from separate evidence.
- **Closed:** must match the calendar, in both directions.
- **Currency:** the report's currency, and the one the raw result states.

Effective figures are labeled and aren't compared to the invoice. When a report has both, the
reconciliation table shows the difference between the billed and effective totals as a calculation.

## Figures and tags

| Kind | Meaning | Tag |
| :--- | :--- | :--- |
| Measured | Read directly from a billing source's saved result. Telemetry can be measured too, for usage counts, never money | `E<n>` |
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
| `FIGURE_KIND` | The evidence can't back that kind of figure, such as measured on a cost-tool allocation, or the figure's basis differs from the one recorded on its evidence |
| `UNRESOLVED_REF` | A reference or tag has no entry |
| `UNTAGGED_MONEY` | Free text holds a money figure. Use a reference |
| `CALC_MISMATCH` | Recomputing gives a different result, or it can't be recomputed |
| `CALC_UNKNOWN` | No such calculation |
| `CALC_CYCLE` | A calculation's inputs lead back to its own output |
| `CALC_KIND` | Marked calculated but uses an assumption or estimate, or an assumption is unstated |
| `CURRENCY_MIX` | One calculation, a driver table, or a reconciliation row mixes currencies |
| `CURRENCY_UNPROVED` | A figure's currency differs from what its raw result or provided entry states |
| `RECON_ROLE` | A reconciliation row holds something other than what it claims. See the rules above |
| `RECON_RESIDUAL` | A residual is missing, or doesn't match its period's invoice and total |
| `CHANGE_SOURCE` | The total change isn't a `change` calculation over the two periods' query totals |
| `DRIVER_SUM` | Drivers don't sum to the total change, or a driver's quantity and rate don't sum to its change. End with an `Other` row |
| `LEDGER_SUM`, `LEDGER_KIND`, `POOL_SUM` | The Kubernetes ledger double-counts or loses cost |
| `OVERLAP` | Two opportunities on one resource are summed, even through a nested sum. Use `overlap_savings` |
| `OPPORTUNITY_FIELD` | An opportunity has no `resource`, or a `type` other than `rightsizing` or `other` |
| `SAVINGS_KIND` | A saving rests on a measured figure, or the total isn't a calculation over the savings |
| `CAUSE_NO_EVIDENCE` | A stated cause cites nothing |
| `PEAK_MISSING` | Rightsizing with no complete telemetry peak, and not marked `needs_investigation` |
| `INVESTIGATE_IN_TOTAL` | An item that needs investigation counts toward the savings total |
| `PAST_TOO_EARLY` | A saving is measured before a complete billing period has passed |
| `PAST_RESULT_CALC` | A measured saving isn't from a `realized_savings` calculation with no assumptions |
| `PAST_WINDOW` | A measured saving's input isn't from the required baseline or comparison month, or its month can't be read |
| `PAST_STATUS` | A past saving's status claims more or less than its result says |
| `STATUS_CLAIM` | A claimed status doesn't hold, or a period's closed flag disagrees with the calendar |
| `RECORD_INVALID` | The record can't be read |
| `RENDER_FAILED` | The record validated but couldn't be rendered |

Fix a failure, or tell the user what can't be proved. A report that failed validation is never shown as
passing.

## What this doesn't prove

Say this plainly when it matters:

- **That a query asked the right question.** Reconciliation and the SQL tests address that, but only
  reconciliation runs on real data.
- **That a cited cause is the true cause.** The validator checks that evidence is cited, not that it
  supports the claim.
- **Figures from outside the bill.** A provided figure is only as good as whoever typed it, and it states
  no period, so a measured saving's months aren't checked for it.
- **That the scope is what it says.** `--scope-kind` is declared by the run, using what discovery shows. A
  member account's run called whole-bill wouldn't be caught unless its invoices disagree.
- **Currency when a raw result doesn't state one.** Without a currency field or column, the label is taken
  as given.
- **Numbers in free text without a currency.** A bare `3600` in a sentence isn't caught, so keep amounts in
  references. Free text is checked for `$`, `€`, `£`, USD, EUR, GBP, and the report's own currency code, so
  an amount in any other currency isn't caught either.
- **Deliberate tampering.** The hashes guard against mistakes, not someone editing files on purpose.

## Period rules

Compare complete, equivalent periods. Exclude days that are incomplete because of export delay, and state
the data-through date. A quiet latest day is not evidence of lower spend. If the date can't be read, leave it
unset rather than typing one: the report says it isn't known, and a past saving can't count as measured.

A saving counts as measured only after a complete billing period since the change. The baseline is the last
complete month before it, and the comparison is the first month starting after it. Each measured input has to
state its month, through its locator or its evidence, so use a query that takes a period.
