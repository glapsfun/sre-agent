# Calculations

All arithmetic runs in `scripts/finops_calc.py`, which uses decimals and never a float. Zero denominators
return `undefined`, and mixed currencies are refused by the validator. Every figure below is synthetic.

```bash
scripts/finops_calc.py residual --input control=56940.00 --input query_total=56938.72
scripts/finops_calc.py pool_distribution --input pool=300 --input weights=2,1
```

Store the result in the record with its inputs as references. The validator recomputes it.

```json
{"id": "C1", "name": "residual", "kind": "calculated", "assumptions": [],
 "inputs": {"control": "oct_invoice", "query_total": "oct_query_total"},
 "outputs": {"residual": "1.28"}}
```

An input that isn't a figure is a literal with its assumption, which makes the calculation `estimated`:
`{"literal": "0.4", "assumption": "40% of NAT bytes are S3"}`.

## The calculations

| Name | Inputs | Outputs | Use it for |
| :--- | :--- | :--- | :--- |
| `residual` | `control`, `query_total` | `residual` | Invoice minus query total, or an `Other` driver row |
| `change` | `before`, `after` | `change`, `pct` | Period over period |
| `share` | `part`, `whole` | `pct` | No-owner share, a scope's share of the bill |
| `sum` | `terms` (list) | `total` | Several invoices, savings totals |
| `product` | `factors` (list) | `total` | Usage times rate times an assumed fraction |
| `qty_rate` | `q0`, `r0`, `q1`, `r1` | `quantity`, `rate`, `change` | Splitting a change for one stable SKU |
| `unit_cost` | `cost`, `units` | `unit_cost` | Cost per order, when the user names the denominator |
| `overlap_savings` | `baseline`, `reductions` (list) | `new_cost`, `savings`, `savings_pct` | Two changes to the same cost |
| `net_benefit` | `savings`, `recurring` | `net` | Savings after new running costs |
| `payback` | `implementation`, `net_monthly` | `months` | Simple payback |
| `horizon_net` | `net_monthly`, `months`, `implementation` | `total` | A flat scenario over a term |
| `commitment_breakeven` | `discount` | `utilization` | Where a commitment starts to pay |
| `commitment_benefit` | `on_demand_rate`, `discount`, `utilization` | `committed`, `consumed`, `benefit` | What a commitment really returned |
| `pool_distribution` | `pool`, `weights` (list) | `share_1..n`, `total` | Distributing shared or idle cost |
| `integrate` | `rates` (list), `hours` (list) | `total` | Varying hourly rates |
| `realized_savings` | `baseline_cost`, `post_cost`, optional `baseline_usage`, `post_usage` | `saving`, `normalized` | Whether a past saving landed |
| `ledger` | `rows` (list), `covered` | `sum`, `residual`, `residual_pct` | The Kubernetes ledger |

## What to get right

**Spend and unit cost can move apart.** Month A costs $12,000 for 600,000 orders, or $0.020 each. Month B
costs $15,000 for 1,000,000 orders, or $0.015 each. Spend rose 25% and unit cost fell 25%, which can be a good
outcome.

**Combined unit cost is total over total.** $100 for 10 transactions and $900 for 300 is 1000 / 310, about
$3.23. Averaging $10 and $3 would give a misleading $6.50. Use `sum` twice, then `unit_cost`.

**Quantity and rate.** `(q1 - q0) * r0 + q1 * (r1 - r0)`. From 100 units at $2 to 120 at $2.50, the $100 increase
is $40 of quantity and $60 of rate. With several SKUs, use consistent units and treat service mix explicitly.
Keep credits and fees as separate components.

**Overlapping savings multiply.** On $10,000, cutting usage 20% and then rate 30% leaves $5,600. That saves $4,400,
or 44%, not 50%. With $200 a month of new tooling the net is $4,200, and a $6,000 implementation pays back in about
1.43 months. Over twelve flat months that's $44,400. Label it estimated until the post-change bill supports it.

**A commitment's discount isn't its saving.** In a simplified single-rate model, a 30% discount breaks even at 70%
utilization. Paying $0.70 an hour for capacity used at 80% compares with $0.80 on demand, a $0.10 benefit. Real
purchase advice needs hourly data, program terms, and eligibility, so report coverage and utilization and don't
recommend a purchase.

**Integrate rates.** `SUM(rate * hours)`. One sample multiplied by a horizon assumes a constant rate.

**Realized savings.** The baseline is the last complete month before the change, and the comparison is the first
month starting after it, both limited to the charges the change targets. With a usage figure:
`baseline cost / baseline usage * post usage - post cost`. Baseline $3,000 for 1,000 units, then $2,664 for 1,200
units, saved $936. Without one: `baseline cost - post cost`, marked not normalized for usage. If the targeted charges
can't be isolated, the status is "cannot measure".

[Unit economics](https://www.finops.org/framework/capabilities/unit-economics/)
