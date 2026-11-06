#!/usr/bin/env python3
"""Decimal calculations for finops reports.

Takes and returns decimal strings so no value ever passes through a float. The
validator recomputes every calculation in a run from its inputs, so results here
are exact and unrounded; rounding happens only when a report is displayed.
"""
from __future__ import annotations

import argparse
import calendar
import json
import sys
from dataclasses import dataclass
from datetime import date
from decimal import ROUND_HALF_UP, Decimal, InvalidOperation, getcontext
from typing import Callable

getcontext().prec = 50

UNDEFINED = "undefined"
HUNDRED = Decimal(100)
Result = dict  # output name to Decimal, or to a plain string such as UNDEFINED


class CalcError(Exception):
    """An input that can't be calculated with, as opposed to a zero denominator."""


@dataclass(frozen=True)
class Calc:
    formula: str
    scalars: tuple[str, ...]
    lists: tuple[str, ...]
    outputs: tuple[str, ...]
    fn: Callable[..., Result]
    optional: tuple[str, ...] = ()


def _ratio(numerator: Decimal, denominator: Decimal):
    return UNDEFINED if denominator == 0 else numerator / denominator


def _pct(numerator: Decimal, denominator: Decimal):
    return UNDEFINED if denominator == 0 else numerator / denominator * HUNDRED


def _residual(control, query_total):
    return {"residual": control - query_total}


def _change(before, after):
    # A credit-heavy month can have a negative baseline, so the base is absolute.
    return {"change": after - before, "pct": _pct(after - before, abs(before))}


def _share(part, whole):
    return {"pct": _pct(part, whole)}


def _sum(terms):
    return {"total": sum(terms, Decimal(0))}


def _product(factors):
    total = Decimal(1)
    for factor in factors:
        total *= factor
    return {"total": total}


def _qty_rate(q0, r0, q1, r1):
    quantity = (q1 - q0) * r0
    rate = q1 * (r1 - r0)
    return {"quantity": quantity, "rate": rate, "change": quantity + rate}


def _unit_cost(cost, units):
    return {"unit_cost": _ratio(cost, units)}


def _overlap_savings(baseline, reductions):
    new_cost = baseline
    for reduction in reductions:
        new_cost *= Decimal(1) - reduction
    savings = baseline - new_cost
    return {"new_cost": new_cost, "savings": savings, "savings_pct": _pct(savings, baseline)}


def _net_benefit(savings, recurring):
    return {"net": savings - recurring}


def _payback(implementation, net_monthly):
    # A change that loses money every month never pays back.
    months = UNDEFINED if net_monthly <= 0 else implementation / net_monthly
    return {"months": months}


def _horizon_net(net_monthly, months, implementation):
    return {"total": net_monthly * months - implementation}


def _commitment_breakeven(discount):
    return {"utilization": Decimal(1) - discount}


def _commitment_benefit(on_demand_rate, discount, utilization):
    committed = on_demand_rate * (Decimal(1) - discount)
    consumed = on_demand_rate * utilization
    return {"committed": committed, "consumed": consumed, "benefit": consumed - committed}


def _pool_distribution(pool, weights):
    if any(weight < 0 for weight in weights):
        raise CalcError("weights can't be negative")
    total_weight = sum(weights, Decimal(0))
    names = [f"share_{index}" for index in range(1, len(weights) + 1)]
    if total_weight == 0:
        return {**{name: UNDEFINED for name in names}, "total": UNDEFINED}
    shares = [pool * weight / total_weight for weight in weights[:-1]]
    # The last share takes the remainder, so a repeating decimal can't change the total.
    shares.append(pool - sum(shares, Decimal(0)))
    return {**dict(zip(names, shares)), "total": pool}


def _integrate(rates, hours):
    if len(rates) != len(hours):
        raise CalcError("rates and hours must have the same length")
    return {"total": sum((rate * hour for rate, hour in zip(rates, hours)), Decimal(0))}


def _realized_savings(baseline_cost, post_cost, baseline_usage=None, post_usage=None):
    if (baseline_usage is None) != (post_usage is None):
        raise CalcError("baseline_usage and post_usage must be given together")
    if baseline_usage is None:
        return {"saving": baseline_cost - post_cost, "normalized": "false"}
    if baseline_usage == 0:
        return {"saving": UNDEFINED, "normalized": "true"}
    return {"saving": baseline_cost / baseline_usage * post_usage - post_cost, "normalized": "true"}


def _ledger(rows, covered):
    total = sum(rows, Decimal(0))
    residual = covered - total
    return {"sum": total, "residual": residual, "residual_pct": _pct(residual, covered)}


CALCS: dict[str, Calc] = {
    "residual": Calc("{control} - {query_total}", ("control", "query_total"), (), ("residual",), _residual),
    "change": Calc("{after} - {before}", ("before", "after"), (), ("change", "pct"), _change),
    "share": Calc("{part} / {whole}", ("part", "whole"), (), ("pct",), _share),
    "sum": Calc("{terms}", (), ("terms",), ("total",), _sum),
    "product": Calc("{factors}", (), ("factors",), ("total",), _product),
    "qty_rate": Calc(
        "({q1} - {q0}) * {r0} + {q1} * ({r1} - {r0})",
        ("q0", "r0", "q1", "r1"),
        (),
        ("quantity", "rate", "change"),
        _qty_rate,
    ),
    "unit_cost": Calc("{cost} / {units}", ("cost", "units"), (), ("unit_cost",), _unit_cost),
    "overlap_savings": Calc(
        "{baseline} reduced in sequence by {reductions}",
        ("baseline",),
        ("reductions",),
        ("new_cost", "savings", "savings_pct"),
        _overlap_savings,
    ),
    "net_benefit": Calc("{savings} - {recurring}", ("savings", "recurring"), (), ("net",), _net_benefit),
    "payback": Calc("{implementation} / {net_monthly}", ("implementation", "net_monthly"), (), ("months",), _payback),
    "horizon_net": Calc(
        "{net_monthly} * {months} - {implementation}",
        ("net_monthly", "months", "implementation"),
        (),
        ("total",),
        _horizon_net,
    ),
    "commitment_breakeven": Calc("1 - {discount}", ("discount",), (), ("utilization",), _commitment_breakeven),
    "commitment_benefit": Calc(
        "{on_demand_rate} * {utilization} - {on_demand_rate} * (1 - {discount})",
        ("on_demand_rate", "discount", "utilization"),
        (),
        ("committed", "consumed", "benefit"),
        _commitment_benefit,
    ),
    "pool_distribution": Calc("{pool} split by weights {weights}", ("pool",), ("weights",), ("total",), _pool_distribution),
    "integrate": Calc("sum of rate * hours, rates {rates}, hours {hours}", (), ("rates", "hours"), ("total",), _integrate),
    "realized_savings": Calc(
        "{baseline_cost} / {baseline_usage} * {post_usage} - {post_cost}, or {baseline_cost} - {post_cost} without usage",
        ("baseline_cost", "post_cost"),
        (),
        ("saving", "normalized"),
        _realized_savings,
        optional=("baseline_usage", "post_usage"),
    ),
    "ledger": Calc("{covered} - sum of {rows}", ("covered",), ("rows",), ("sum", "residual", "residual_pct"), _ledger),
}

FORMULAS: dict[str, str] = {name: calc.formula for name, calc in CALCS.items()}


def _to_decimal(label: str, value: object) -> Decimal:
    if not isinstance(value, str):
        raise CalcError(f"{label} must be a decimal string, got {type(value).__name__}")
    try:
        number = Decimal(value)
    except InvalidOperation as exc:
        raise CalcError(f"{label} is not a number: {value!r}") from exc
    if not number.is_finite():
        raise CalcError(f"{label} is not finite: {value!r}")
    return number


def _plain(value) -> str:
    return value if isinstance(value, str) else format(value, "f")


def _output_names(calc: Calc, inputs: dict) -> list[str]:
    if calc.fn is _pool_distribution:
        return [f"share_{index}" for index in range(1, len(inputs["weights"]) + 1)] + ["total"]
    return list(calc.outputs)


def compute(name: str, inputs: dict, currencies: list | None = None) -> dict:
    """Returns output name to decimal string, or "undefined" for a zero denominator."""
    calc = CALCS.get(name)
    if calc is None:
        raise CalcError(f"unknown calculation: {name}")
    if currencies and len(set(currencies)) > 1:
        raise CalcError(f"mixed currencies: {sorted(set(currencies))}")

    allowed = set(calc.scalars) | set(calc.lists) | set(calc.optional)
    unknown = set(inputs) - allowed
    if unknown:
        raise CalcError(f"{name}: unknown inputs {sorted(unknown)}")
    missing = (set(calc.scalars) | set(calc.lists)) - set(inputs)
    if missing:
        raise CalcError(f"{name}: missing inputs {sorted(missing)}")

    undefined = False
    converted: dict = {}
    for key, value in inputs.items():
        if key in calc.lists:
            if not isinstance(value, (list, tuple)):
                raise CalcError(f"{name}: {key} must be a list")
            undefined = undefined or UNDEFINED in value
            converted[key] = [] if UNDEFINED in value else [_to_decimal(key, item) for item in value]
        elif value == UNDEFINED:
            undefined = True
        else:
            converted[key] = _to_decimal(key, value)

    if undefined:
        return {output: UNDEFINED for output in _output_names(calc, inputs)}
    return {key: _plain(value) for key, value in calc.fn(**converted).items()}


def quantize_money(value: str) -> str:
    if value == UNDEFINED:
        return UNDEFINED
    return format(Decimal(value).quantize(Decimal("0.01"), rounding=ROUND_HALF_UP), "f")


def display_number(value: str) -> str:
    """Thousands separators and readable decimals. For display only, never for math."""
    if value == UNDEFINED:
        return UNDEFINED
    text = format(Decimal(value), "f")
    if "." in text:
        whole, fraction = text.split(".")
        if len(fraction) > 2:
            fraction = fraction[:6].rstrip("0").ljust(2, "0")
        text = f"{whole}.{fraction}"
    return format(Decimal(text), ",f")


def fill_formula(name: str, values: dict) -> str:
    """The calculation's formula with its input values filled in."""
    formula = FORMULAS[name]
    if name == "realized_savings":
        if "baseline_usage" in values:
            formula = "{baseline_cost} / {baseline_usage} * {post_usage} - {post_cost}"
        else:
            formula = "{baseline_cost} - {post_cost}, not normalized for usage"
    joiner = {"sum": " + ", "product": " * "}.get(name, ", ")
    filled = {
        key: joiner.join(display_number(item) for item in value) if isinstance(value, list) else display_number(value)
        for key, value in values.items()
    }
    return formula.format(**filled)


def _month_key(year: int, month: int) -> str:
    return f"{year:04d}-{month:02d}"


def _shift_month(year: int, month: int, delta: int) -> tuple[int, int]:
    index = year * 12 + (month - 1) + delta
    return index // 12, index % 12 + 1


def measure_window(implemented_on: date) -> tuple[str, str]:
    """The last complete month before a change, and the first month starting after it."""
    before = _shift_month(implemented_on.year, implemented_on.month, -1)
    after = _shift_month(implemented_on.year, implemented_on.month, 1)
    return _month_key(*before), _month_key(*after)


def too_early(implemented_on: date, data_through: date) -> bool:
    """True until billing data covers the whole first month after the change."""
    year, month = _shift_month(implemented_on.year, implemented_on.month, 1)
    last_day = date(year, month, calendar.monthrange(year, month)[1])
    return data_through < last_day


def _parse_inputs(name: str, pairs: list) -> dict:
    calc = CALCS.get(name)
    if calc is None:
        raise CalcError(f"unknown calculation: {name}")
    inputs: dict = {}
    for pair in pairs:
        key, separator, value = pair.partition("=")
        if not separator:
            raise CalcError(f"--input needs key=value, got {pair!r}")
        inputs[key] = value.split(",") if key in calc.lists else value
    return inputs


def main(argv: list | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Run one finops calculation and print its formula, inputs, and outputs as JSON.",
        epilog="Calculations: " + ", ".join(sorted(CALCS)),
    )
    parser.add_argument("name", help="calculation name")
    parser.add_argument("--input", action="append", default=[], metavar="KEY=VALUE", help="list inputs use commas: weights=2,1")
    args = parser.parse_args(argv)
    try:
        inputs = _parse_inputs(args.name, args.input)
        outputs = compute(args.name, inputs)
    except CalcError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    print(json.dumps({"name": args.name, "formula": FORMULAS[args.name], "inputs": inputs, "outputs": outputs}, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
