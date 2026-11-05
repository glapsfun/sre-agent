#!/usr/bin/env python3
"""Proves a finops run record before a report can be rendered from it.

Re-reads every raw result, recomputes every calculation, checks that totals
conserve, and checks that each figure is the kind of thing its place in the report
claims. This guards against mistakes, not someone deliberately editing files, and it
can't prove that a query asked the right question or that a cited cause is the true
cause.
"""
from __future__ import annotations

import argparse
import hashlib
import re
import sys
from dataclasses import dataclass
from datetime import date, datetime, timezone
from decimal import Decimal, InvalidOperation
from pathlib import Path

from finops_calc import FORMULAS, UNDEFINED, CalcError, compute, too_early
from finops_record import LocatorError, RecordError, is_closed, load_run, locate, raw_currency

STATUS_NOT_RECONCILED = "Not reconciled"
STATUS_OPEN = "Open period"
STATUS_PARTIAL = "Partial scope"
STATUS_CROSS_CHECKED = "Cross-checked only"
STATUS_RECONCILED = "Reconciled to invoice"

PAST_LANDED = "Landed"
PAST_BELOW_ESTIMATE = "Landed, below estimate"
PAST_DID_NOT_LAND = "Did not land"
PAST_CANNOT_MEASURE = "cannot measure"
PAST_UNMEASURED = {"too early to measure", "not yet measured", "approved, not implemented", "rejected", PAST_CANNOT_MEASURE}

LEDGER_ROW_KINDS = {"workload", "shared", "idle", "unallocated", "out_of_cluster"}
OPPORTUNITY_TYPES = {"rightsizing", "other"}
NON_MONEY_OUTPUTS = {"pct", "savings_pct", "residual_pct", "months", "utilization", "normalized"}
QUANTITY_COLUMNS = {"quantity", "usage_amount"}
MEASURED_EVIDENCE = {"billing", "telemetry"}
ESTIMATED_EVIDENCE = {"recommendation", "allocation", "prior_run"}
SAVINGS_TOTAL_CALCS = {"sum", "overlap_savings", "net_benefit"}

_CALC_REF = re.compile(r"^(C\d+)\.(\w+)$")
_MONEY = re.compile(r"[$€£]\s?\d|\b(?:USD|EUR|GBP)\s?\d|\d\s?(?:USD|EUR|GBP)\b")
_PERIOD_VALUE = re.compile(r"^\d{4}-\d{2}")


class RefError(Exception):
    """A reference that doesn't resolve to a figure or a calculation output."""


@dataclass(frozen=True)
class Failure:
    code: str
    where: str
    message: str


@dataclass(frozen=True)
class Resolved:
    value: str
    currency: str | None
    tag: str
    estimated: bool


def _find_calc(run: dict, calc_id: str) -> dict | None:
    return next((calc for calc in run.get("calculations", []) if calc.get("id") == calc_id), None)


def _is_literal(item: object) -> bool:
    return isinstance(item, dict) and "literal" in item


def _input_items(calc: dict) -> list:
    items: list = []
    for value in calc.get("inputs", {}).values():
        items.extend(value if isinstance(value, list) else [value])
    return items


def _calc_currency(run: dict, calc: dict, seen: frozenset = frozenset()) -> str | None:
    if calc.get("id") in seen:
        return None
    seen = seen | {calc.get("id")}
    for item in _input_items(calc):
        if _is_literal(item):
            continue
        currency = _ref_currency(run, item, seen)
        if currency:
            return currency
    return None


def _ref_currency(run: dict, ref: str, seen: frozenset = frozenset()) -> str | None:
    match = _CALC_REF.match(ref) if isinstance(ref, str) else None
    if match:
        calc = _find_calc(run, match.group(1))
        if calc is None or match.group(2) in NON_MONEY_OUTPUTS:
            return None
        return _calc_currency(run, calc, seen)
    figure = run.get("figures", {}).get(ref)
    return figure.get("currency") if figure else None


def resolve(run: dict, ref: str) -> Resolved:
    """Turns a figure ID or C<n>.<output> into its value and report tag."""
    if not isinstance(ref, str):
        raise RefError(f"not a reference: {ref!r}")
    match = _CALC_REF.match(ref)
    if match:
        calc = _find_calc(run, match.group(1))
        if calc is None:
            raise RefError(f"no calculation {match.group(1)}")
        outputs = calc.get("outputs", {})
        if match.group(2) not in outputs:
            raise RefError(f"{match.group(1)} has no output {match.group(2)}")
        estimated = calc.get("kind") == "estimated"
        tag = f"{calc['id']} est" if estimated else calc["id"]
        return Resolved(outputs[match.group(2)], _ref_currency(run, ref), tag, estimated)

    figure = run.get("figures", {}).get(ref)
    if figure is None:
        raise RefError(f"no figure {ref}")
    if figure.get("kind") == "provided":
        return Resolved(figure["value"], figure.get("currency"), figure.get("provided", "?"), False)
    estimated = figure.get("kind") == "estimated"
    source = figure.get("evidence", "?")
    return Resolved(figure["value"], figure.get("currency"), f"{source} est" if estimated else source, estimated)


def input_values(run: dict, calc: dict) -> dict:
    """The calculation's inputs as compute() takes them. Raises RefError."""

    def value_of(item: object) -> str:
        return item["literal"] if _is_literal(item) else resolve(run, item).value

    values: dict = {}
    for key, item in calc.get("inputs", {}).items():
        values[key] = [value_of(entry) for entry in item] if isinstance(item, list) else value_of(item)
    return values


def _number(value: str | None) -> Decimal | None:
    if value is None or value == UNDEFINED:
        return None
    try:
        return Decimal(value)
    except InvalidOperation:
        return None


def _same(left: str, right: str) -> bool:
    a, b = _number(left), _number(right)
    return left == right if a is None or b is None else a == b


def _within(control: Decimal, value: Decimal, tolerance_pct: Decimal) -> bool:
    if control == 0:
        return value == 0
    return abs(control - value) / abs(control) * 100 <= tolerance_pct


def _ref_number(run: dict, ref: str | None) -> Decimal | None:
    if ref is None:
        return None
    try:
        return _number(resolve(run, ref).value)
    except RefError:
        return None


def derive_status(run: dict) -> tuple:
    """The report's status and reason. It is computed, never written by the model."""
    scope = run["scope"]
    tolerance = Decimal(scope["tolerance_pct"])
    partial = scope.get("scope_kind") == "partial" or "_" in (scope.get("period") or "")
    entries = run.get("reconciliation", [])
    if not entries and not partial:
        return STATUS_NOT_RECONCILED, "no reconciliation was done"

    problems: list = []
    open_periods: list = []
    cross_checked: list = []
    if not partial:
        covered = {entry.get("period") for entry in entries}
        for period in (scope.get("period"), scope.get("comparison_period")):
            if period and period not in covered:
                problems.append(f"{period}: no reconciliation entry")
    for entry in entries:
        period = entry.get("period")
        if not entry.get("closed"):
            open_periods.append(period)
            continue
        if partial:
            continue
        query = _ref_number(run, entry.get("query_total"))
        invoice = _ref_number(run, entry.get("invoice"))
        second = _ref_number(run, entry.get("second_view"))
        if query is None:
            problems.append(f"{period}: no query total")
        elif invoice is not None:
            if not _within(invoice, query, tolerance):
                problems.append(f"{period}: residual is outside {tolerance}% of the invoice")
        elif second is None:
            problems.append(f"{period}: no invoice figure and no second billing view")
        elif _within(second, query, tolerance):
            cross_checked.append(period)
        else:
            problems.append(f"{period}: second billing view differs by more than {tolerance}%")

    if problems:
        return STATUS_NOT_RECONCILED, "; ".join(problems)
    if open_periods:
        return STATUS_OPEN, f"{', '.join(map(str, open_periods))} not closed, figures are provisional"
    if partial:
        return STATUS_PARTIAL, "the scope is less than a whole bill, so it can't match an invoice alone"
    if cross_checked:
        return STATUS_CROSS_CHECKED, f"no invoice figure for {', '.join(map(str, cross_checked))}"
    return STATUS_RECONCILED, f"within {tolerance}% of each invoice"


def past_status(run: dict, item: dict) -> str:
    """What a measured saving's result says, so a claim can't outrun its number."""
    saving = _ref_number(run, item.get("result"))
    if saving is None:
        return PAST_CANNOT_MEASURE
    if saving <= 0:
        return PAST_DID_NOT_LAND
    estimate = _ref_number(run, item.get("estimated"))
    return PAST_BELOW_ESTIMATE if estimate is not None and saving < estimate else PAST_LANDED


class _Checker:
    def __init__(self, run_dir: Path, run: dict):
        self.run_dir = run_dir
        self.run = run
        self.failures: list = []
        self.evidence = {entry.get("id"): entry for entry in run.get("evidence", [])}

    def fail(self, code: str, where: str, message: str) -> None:
        self.failures.append(Failure(code, where, message))

    def check_ref(self, where: str, ref: object) -> None:
        if ref is None:
            return
        try:
            resolve(self.run, ref)
        except RefError as exc:
            self.fail("UNRESOLVED_REF", where, str(exc))

    def check_evidence_id(self, where: str, evidence_id: object) -> None:
        if evidence_id not in self.evidence:
            self.fail("UNRESOLVED_REF", where, f"no evidence {evidence_id}")

    def check_text(self, where: str, text: object) -> None:
        if isinstance(text, str) and _MONEY.search(text):
            self.fail("UNTAGGED_MONEY", where, "free text holds a money figure; use a figure or calculation reference")

    def all_refs(self, ref: object) -> set:
        """A reference and every reference a calculation behind it rests on."""
        seen: set = set()
        stack = [ref]
        while stack:
            current = stack.pop()
            if not isinstance(current, str) or current in seen:
                continue
            seen.add(current)
            match = _CALC_REF.match(current)
            calc = _find_calc(self.run, match.group(1)) if match else None
            if calc:
                stack.extend(item for item in _input_items(calc) if not _is_literal(item))
        return seen

    def raw_results(self) -> None:
        for entry in self.run.get("evidence", []):
            where = f"evidence.{entry.get('id')}"
            path = self.run_dir / entry.get("raw_path", "")
            if not path.is_file():
                self.fail("RAW_MISSING", where, f"{entry.get('raw_path')} is missing")
            elif hashlib.sha256(path.read_bytes()).hexdigest() != entry.get("sha256"):
                self.fail("HASH_MISMATCH", where, f"{entry.get('raw_path')} changed after it was saved")

    def figures(self) -> None:
        for name, figure in self.run.get("figures", {}).items():
            where = f"figures.{name}"
            kind = figure.get("kind")
            if _number(figure.get("value")) is None:
                self.fail("FIGURE_MISMATCH", where, f"{figure.get('value')!r} isn't a number")
            elif kind == "provided":
                self.provided_figure(where, figure)
            elif kind in {"measured", "estimated"}:
                self.located_figure(where, figure, kind)
            else:
                self.fail("FIGURE_KIND", where, f"unknown figure kind {kind!r}")

    def provided_figure(self, where: str, figure: dict) -> None:
        entry = next((p for p in self.run.get("provided", []) if p.get("id") == figure.get("provided")), None)
        if entry is None:
            self.fail("UNRESOLVED_REF", where, f"no provided figure {figure.get('provided')}")
            return
        if not _same(figure.get("value", ""), entry.get("value", "")):
            self.fail("FIGURE_MISMATCH", where, f"{figure.get('value')} differs from provided {entry.get('value')}")
        if figure.get("currency") != entry.get("currency"):
            self.fail("CURRENCY_UNPROVED", where, f"{figure.get('currency')} differs from the {entry.get('currency')} that was provided")

    def located_figure(self, where: str, figure: dict, kind: str) -> None:
        entry = self.evidence.get(figure.get("evidence"))
        if entry is None:
            self.fail("UNRESOLVED_REF", where, f"no evidence {figure.get('evidence')}")
            return
        allowed = MEASURED_EVIDENCE if kind == "measured" else ESTIMATED_EVIDENCE
        if entry.get("kind") not in allowed:
            self.fail("FIGURE_KIND", where, f"a {kind} figure can't rest on {entry.get('kind')} evidence")
        if entry.get("kind") == "telemetry" and figure.get("currency"):
            self.fail("FIGURE_KIND", where, "telemetry can back a usage count, but not a money figure")
        if not entry.get("complete", False):
            self.fail("EVIDENCE_INCOMPLETE", where, f"{entry.get('id')} is incomplete and can't back a figure")
        path = self.run_dir / entry.get("raw_path", "")
        if not path.is_file():
            return
        locator = figure.get("locator", {})
        try:
            found = locate(path, locator)
        except LocatorError as exc:
            self.fail("LOCATOR_FAILED", where, str(exc))
            return
        if not _same(figure.get("value", ""), found):
            self.fail("FIGURE_MISMATCH", where, f"{figure.get('value')} differs from {found} in {entry.get('raw_path')}")
        stated = figure.get("currency")
        proved = raw_currency(path, locator)
        quantity = stated is None and locator.get("column") in QUANTITY_COLUMNS
        if proved and stated != proved and not quantity:
            self.fail("CURRENCY_UNPROVED", where, f"labeled {stated}, but {entry.get('raw_path')} says {proved}")

    def calculations(self) -> None:
        for calc in self.run.get("calculations", []):
            self.calculation(calc)

    def calculation(self, calc: dict) -> None:
        where = f"calculations.{calc.get('id')}"
        name = calc.get("name")
        if name not in FORMULAS:
            self.fail("CALC_UNKNOWN", where, f"unknown calculation {name!r}")
            return
        try:
            values = input_values(self.run, calc)
        except RefError as exc:
            self.fail("UNRESOLVED_REF", where, str(exc))
            return

        items = _input_items(calc)
        currencies = {_ref_currency(self.run, item) for item in items if not _is_literal(item)} - {None}
        if len(currencies) > 1:
            self.fail("CURRENCY_MIX", where, f"mixes {sorted(currencies)}")

        has_literal = any(_is_literal(item) for item in items)
        has_estimate = any(resolve(self.run, item).estimated for item in items if not _is_literal(item))
        if calc.get("kind") == "calculated" and (has_literal or has_estimate):
            self.fail("CALC_KIND", where, "uses an assumption or an estimate, so it must be estimated")
        elif calc.get("kind") not in {"calculated", "estimated"}:
            self.fail("CALC_KIND", where, f"unknown calculation kind {calc.get('kind')!r}")
        for item in items:
            if _is_literal(item):
                self.check_text(where, item.get("assumption"))
                if not item.get("assumption"):
                    self.fail("CALC_KIND", where, f"literal {item.get('literal')} has no assumption")
        for assumption in calc.get("assumptions", []):
            self.check_text(where, assumption)

        try:
            expected = compute(name, values)
        except CalcError as exc:
            self.fail("CALC_MISMATCH", where, f"can't recompute: {exc}")
            return
        stored = calc.get("outputs", {})
        if set(expected) != set(stored) or any(not _same(expected[key], stored[key]) for key in expected):
            self.fail("CALC_MISMATCH", where, f"stored outputs differ from recomputed {expected}")

    def reconciliation(self) -> None:
        today = datetime.now(timezone.utc).date()
        fields = ("invoice", "second_view", "query_total", "residual", "no_owner", "no_owner_share", "effective_total", "billed_effective_difference", "whole_bill_total", "scope_share")
        for entry in self.run.get("reconciliation", []):
            period = entry.get("period")
            where = f"reconciliation.{period}"
            for field in fields:
                self.check_ref(f"{where}.{field}", entry.get(field))
            self.closed_flag(where, entry, today)
            self.invoice_role(where, entry)
            self.query_role(f"{where}.query_total", entry.get("query_total"), period)
            self.second_view_role(where, entry)
            self.reconciliation_currency(where, entry)

            invoice = _ref_number(self.run, entry.get("invoice"))
            control = invoice if invoice is not None else _ref_number(self.run, entry.get("second_view"))
            query = _ref_number(self.run, entry.get("query_total"))
            residual = _ref_number(self.run, entry.get("residual"))
            if invoice is not None and query is not None and entry.get("residual") is None:
                self.fail("RECON_RESIDUAL", where, "an invoice and a query total need a residual, which is always printed")
            if control is not None and query is not None and residual is not None and residual != control - query:
                self.fail("RECON_RESIDUAL", where, f"residual {residual} isn't {control - query}")

    def closed_flag(self, where: str, entry: dict, today: date) -> None:
        try:
            ended = is_closed(str(entry.get("period")), today)
        except ValueError:
            self.fail("STATUS_CLAIM", where, f"unreadable period {entry.get('period')!r}")
            return
        if ended and not entry.get("closed"):
            self.fail("STATUS_CLAIM", where, "marked open, but the period has ended")
        elif entry.get("closed") and not ended:
            self.fail("STATUS_CLAIM", where, "marked closed, but the period hasn't ended")

    def invoice_role(self, where: str, entry: dict) -> None:
        """An invoice is provided by a person, or read from invoice evidence, and counts every invoice."""
        ref = entry.get("invoice")
        if ref is None:
            return
        match = _CALC_REF.match(ref) if isinstance(ref, str) else None
        names: list = [ref]
        if match:
            calc = _find_calc(self.run, match.group(1))
            terms = calc.get("inputs", {}).get("terms") if calc and calc.get("name") == "sum" else None
            if not isinstance(terms, list) or not all(isinstance(term, str) and not _CALC_REF.match(term) for term in terms):
                self.fail("RECON_ROLE", where, "an invoice is a figure, or a sum of invoice figures")
                return
            names = terms

        seen: dict = {}
        for name in names:
            figure = self.run.get("figures", {}).get(name)
            if figure is None:
                self.fail("RECON_ROLE", where, f"{name} isn't an invoice figure")
                return
            if figure.get("kind") == "provided":
                continue
            evidence = self.evidence.get(figure.get("evidence"), {})
            if figure.get("kind") != "measured" or evidence.get("role") != "invoice":
                self.fail("RECON_ROLE", where, f"{name} wasn't read from invoice evidence, so comparing it would only compare two billing views")
                return
            if evidence.get("period") != entry.get("period"):
                self.fail("RECON_ROLE", where, f"{name} is the invoice for {evidence.get('period')}")
            if len(evidence.get("currencies", [])) > 1:
                self.fail("RECON_ROLE", where, f"{evidence.get('id')} holds invoices in {', '.join(evidence['currencies'])}, which can't be summed")
            path = figure.get("locator", {}).get("path", [])
            seen.setdefault(evidence.get("id"), set()).add(tuple(path[:2]))
        for evidence_id, invoices in seen.items():
            rows = self.evidence[evidence_id].get("rows")
            if rows is not None and len(invoices) != rows:
                self.fail("RECON_ROLE", where, f"uses {len(invoices)} of the {rows} invoices in {evidence_id}")

    def query_role(self, where: str, ref: object, period: object) -> None:
        """A query total is billed cost, read from a query result, for this period."""
        if ref is None:
            return
        figure = self.run.get("figures", {}).get(ref) if isinstance(ref, str) else None
        if figure is None or figure.get("kind") != "measured":
            self.fail("RECON_ROLE", where, f"{ref} must be a measured figure read from a query result")
            return
        if figure.get("basis") != "billed":
            self.fail("RECON_ROLE", where, f"{ref} is on {figure.get('basis')} basis, and only billed cost can reconcile")
        evidence = self.evidence.get(figure.get("evidence"), {})
        if evidence.get("role") != "query":
            self.fail("RECON_ROLE", where, f"{ref} wasn't read from a query result")
            return
        where_values = (figure.get("locator", {}).get("where") or {}).values()
        named = [value for value in where_values if isinstance(value, str) and _PERIOD_VALUE.match(value)]
        if period not in named if named else evidence.get("period") != period:
            self.fail("RECON_ROLE", where, f"{ref} is for another period than {period}")

    def second_view_role(self, where: str, entry: dict) -> None:
        ref = entry.get("second_view")
        if ref is None:
            return
        self.query_role(f"{where}.second_view", ref, entry.get("period"))
        figures = self.run.get("figures", {})
        first = figures.get(entry.get("query_total"), {}) if isinstance(entry.get("query_total"), str) else {}
        second = figures.get(ref, {}) if isinstance(ref, str) else {}
        if first.get("evidence") == second.get("evidence"):
            self.fail("RECON_ROLE", where, "a second billing view must be separate evidence from the query total")

    def reconciliation_currency(self, where: str, entry: dict) -> None:
        expected = self.run["scope"].get("currency")
        for field in ("invoice", "second_view", "query_total"):
            ref = entry.get(field)
            if ref is None:
                continue
            currency = _ref_currency(self.run, ref)
            if currency != expected:
                self.fail("CURRENCY_MIX", f"{where}.{field}", f"{ref} is in {currency}, and this report is in {expected}")

    def changes(self) -> None:
        changes = self.run.get("changes")
        if not changes:
            return
        if changes.get("total") is None:
            self.fail("UNRESOLVED_REF", "changes.total", "the total change is required")
        self.check_ref("changes.total", changes.get("total"))
        self.check_ref("changes.pct", changes.get("pct"))
        self.change_source(changes)
        total = Decimal(0)
        currencies = set()
        for index, driver in enumerate(changes.get("drivers", [])):
            where = f"changes.drivers.{index}"
            self.check_text(where, driver.get("name"))
            for field in ("quantity", "rate", "change"):
                self.check_ref(f"{where}.{field}", driver.get(field))
            amount = _ref_number(self.run, driver.get("change"))
            total += amount if amount is not None else Decimal(0)
            currencies.add(_ref_currency(self.run, driver.get("change")))
            quantity = _ref_number(self.run, driver.get("quantity"))
            rate = _ref_number(self.run, driver.get("rate"))
            if quantity is not None and rate is not None and amount is not None and quantity + rate != amount:
                self.fail("DRIVER_SUM", where, f"quantity {quantity} and rate {rate} don't add up to {amount}")
            cause = driver.get("cause")
            if cause is not None:
                self.check_text(where, cause.get("text"))
                if not cause.get("evidence"):
                    self.fail("CAUSE_NO_EVIDENCE", where, "a stated cause must cite evidence")
                for evidence_id in cause.get("evidence", []):
                    self.check_evidence_id(where, evidence_id)
        currencies.discard(None)
        if len(currencies) > 1:
            self.fail("CURRENCY_MIX", "changes.drivers", f"drivers mix {sorted(currencies)}")
        expected = _ref_number(self.run, changes.get("total"))
        if expected is not None and total != expected:
            self.fail("DRIVER_SUM", "changes.drivers", f"drivers sum to {total}, the total change is {expected}")

    def change_source(self, changes: dict) -> None:
        """The headline change compares the two periods' query totals, and nothing else."""
        total = changes.get("total")
        match = _CALC_REF.match(total) if isinstance(total, str) else None
        calc = _find_calc(self.run, match.group(1)) if match else None
        if calc is None or calc.get("name") != "change" or match.group(2) != "change":
            self.fail("CHANGE_SOURCE", "changes.total", "the total change must be a change calculation over the query totals")
            return
        if changes.get("pct") != f"{calc['id']}.pct":
            self.fail("CHANGE_SOURCE", "changes.pct", f"the percentage must come from {calc['id']}")
        totals = {entry.get("period"): entry.get("query_total") for entry in self.run.get("reconciliation", [])}
        scope = self.run["scope"]
        inputs = calc.get("inputs", {})
        if inputs.get("after") != totals.get(scope.get("period")) or inputs.get("before") != totals.get(scope.get("comparison_period")):
            self.fail("CHANGE_SOURCE", "changes.total", "the total change must compare the comparison period's query total with this period's")

    def ledger(self) -> None:
        ledger = self.run.get("kubernetes_ledger")
        if not ledger:
            return
        self.check_ref("kubernetes_ledger.covered", ledger.get("covered"))
        covered = self.run.get("figures", {}).get(ledger.get("covered"), {})
        if covered.get("kind") not in {"measured", "provided"}:
            self.fail("LEDGER_SUM", "kubernetes_ledger.covered", "covered charges must come from the bill, not a cost tool or a calculation")
        rows = ledger.get("rows", [])
        for index, row in enumerate(rows):
            where = f"kubernetes_ledger.rows.{index}"
            self.check_ref(where, row.get("amount"))
            self.check_text(where, row.get("name"))
            if row.get("kind") not in LEDGER_ROW_KINDS:
                self.fail("LEDGER_KIND", where, f"{row.get('kind')!r} can't be a ledger row, or node charges would count twice")

        calc = _find_calc(self.run, ledger.get("calc"))
        if calc is None or calc.get("name") != "ledger":
            self.fail("LEDGER_SUM", "kubernetes_ledger.calc", f"{ledger.get('calc')} isn't a ledger calculation")
        else:
            counted = sorted(row.get("amount", "") for row in rows if not row.get("distributed"))
            if counted != sorted(calc.get("inputs", {}).get("rows", [])):
                self.fail("LEDGER_SUM", "kubernetes_ledger.rows", "the rows counted differ from the ledger calculation's rows")
            if calc.get("inputs", {}).get("covered") != ledger.get("covered"):
                self.fail("LEDGER_SUM", "kubernetes_ledger.covered", "covered charges differ from the ledger calculation's input")

        for index, distribution in enumerate(ledger.get("distributions", [])):
            self.distribution(f"kubernetes_ledger.distributions.{index}", distribution, rows)

    def distribution(self, where: str, distribution: dict, rows: list) -> None:
        calc = _find_calc(self.run, distribution.get("calc"))
        if calc is None or calc.get("name") != "pool_distribution":
            self.fail("POOL_SUM", where, f"{distribution.get('calc')} isn't a pool distribution")
            return
        pool = distribution.get("pool")
        sources = distribution.get("from_rows", [pool])
        amounts = {row.get("amount"): row for row in rows}
        for source in sources:
            if not amounts.get(source, {}).get("distributed"):
                self.fail("POOL_SUM", where, f"pool row {source} still counts after being distributed, so its cost counts twice")
        values = [_ref_number(self.run, source) for source in sources]
        pool_value = _ref_number(self.run, pool)
        if None not in values and pool_value is not None and sum(values, Decimal(0)) != pool_value:
            self.fail("POOL_SUM", where, f"source rows sum to {sum(values, Decimal(0))}, the pool is {pool_value}")
        for output in calc.get("outputs", {}):
            if output.startswith("share_") and f"{calc.get('id')}.{output}" not in amounts:
                self.fail("POOL_SUM", where, f"{calc.get('id')}.{output} isn't a ledger row, so part of the pool is lost")

    def saving_is_an_estimate(self, ref: object) -> bool:
        """A saving is modeled, so it's estimated or stated by a person, never read off the bill."""
        try:
            if resolve(self.run, ref).estimated:
                return True
        except RefError:
            return True  # Reported as UNRESOLVED_REF already.
        leaves = [item for item in self.all_refs(ref) if not _CALC_REF.match(item)]
        figures = self.run.get("figures", {})
        return bool(leaves) and all(figures.get(item, {}).get("kind") == "provided" for item in leaves)

    def opportunities(self) -> None:
        total_ref = self.run.get("savings_total")
        self.check_ref("savings_total", total_ref)
        total_refs = self.all_refs(total_ref) if total_ref else set()
        match = _CALC_REF.match(total_ref) if isinstance(total_ref, str) else None
        calc = _find_calc(self.run, match.group(1)) if match else None
        if total_ref is not None and (calc is None or calc.get("name") not in SAVINGS_TOTAL_CALCS):
            self.fail("SAVINGS_KIND", "savings_total", "the savings total must be a calculation over the opportunities")

        by_resource: dict = {}
        for item in self.run.get("opportunities", []):
            where = f"opportunities.{item.get('id')}"
            monthly = item.get("monthly")
            self.check_ref(f"{where}.monthly", monthly)
            if not self.saving_is_an_estimate(monthly):
                self.fail("SAVINGS_KIND", where, "a saving is modeled, so it must be estimated or provided")
            if item.get("type") not in OPPORTUNITY_TYPES:
                self.fail("OPPORTUNITY_FIELD", where, f"type must be one of {sorted(OPPORTUNITY_TYPES)}")
            if not item.get("resource"):
                self.fail("OPPORTUNITY_FIELD", where, "resource is required, so overlapping savings can be found")
            for field in ("action", "owner"):
                self.check_text(where, item.get(field))
            for assumption in item.get("assumes", []):
                self.check_text(where, assumption)
            reliability = item.get("reliability") or {}
            self.check_text(where, reliability.get("text"))
            for evidence_id in reliability.get("evidence", []):
                self.check_evidence_id(where, evidence_id)

            if item.get("type") == "rightsizing" and not item.get("needs_investigation"):
                peak = self.evidence.get(item.get("peak_evidence"))
                if peak is None or peak.get("kind") != "telemetry" or not peak.get("complete", False):
                    self.fail("PEAK_MISSING", where, "rightsizing needs complete telemetry evidence of peak demand, or needs_investigation")
            if item.get("needs_investigation") and monthly in total_refs:
                self.fail("INVESTIGATE_IN_TOTAL", where, "an item that needs investigation can't count toward the savings total")

            overlap = item.get("overlap_calc")
            if overlap is not None:
                overlap_calc = _find_calc(self.run, overlap)
                if overlap_calc is None or overlap_calc.get("name") != "overlap_savings":
                    self.fail("OVERLAP", where, f"{overlap} isn't an overlap_savings calculation")
            if item.get("resource") and monthly in total_refs:
                by_resource.setdefault(item["resource"], []).append(item.get("id"))

        for resource, ids in by_resource.items():
            if len(ids) > 1:
                self.fail("OVERLAP", "savings_total", f"{', '.join(ids)} target {resource} and are summed; use an overlap_savings calculation")

    def past_savings(self) -> None:
        for item in self.run.get("past_savings", []):
            where = f"past_savings.{item.get('id')}"
            for field in ("estimated", "result", "avoidance"):
                self.check_ref(f"{where}.{field}", item.get(field))
            for field in ("action", "status"):
                self.check_text(where, item.get(field))
            result = item.get("result")
            if result is None:
                if item.get("status") not in PAST_UNMEASURED:
                    self.fail("PAST_STATUS", where, f"{item.get('status')!r} claims a result, and there is none")
                continue
            match = _CALC_REF.match(result) if isinstance(result, str) else None
            calc = _find_calc(self.run, match.group(1)) if match else None
            if calc is None or calc.get("name") != "realized_savings" or calc.get("kind") != "calculated":
                self.fail("PAST_RESULT_CALC", where, "a measured saving must come from a realized_savings calculation with no assumptions")
            elif item.get("status") != past_status(self.run, item):
                self.fail("PAST_STATUS", where, f"the result says {past_status(self.run, item)!r}, not {item.get('status')!r}")
            data_through = self.run["scope"].get("data_through")
            try:
                implemented = date.fromisoformat(item.get("implemented_on", ""))
            except (TypeError, ValueError):
                self.fail("PAST_TOO_EARLY", where, f"unreadable implemented_on {item.get('implemented_on')!r}")
                continue
            if not data_through or too_early(implemented, date.fromisoformat(data_through)):
                self.fail("PAST_TOO_EARLY", where, "no complete billing period is known to have passed since the change")

    def gaps(self) -> None:
        for index, item in enumerate(self.run.get("gaps", [])):
            where = f"gaps.{index}"
            if isinstance(item, dict):
                self.check_text(where, item.get("text"))
                if item.get("ref") is None:
                    self.fail("UNRESOLVED_REF", where, "a gap object needs a ref; use plain text if it has no amount")
                self.check_ref(where, item.get("ref"))
            else:
                self.check_text(where, item)

    def status(self) -> None:
        claimed = self.run.get("status")
        if claimed is not None and claimed != derive_status(self.run)[0]:
            self.fail("STATUS_CLAIM", "status", f"claims {claimed!r}, the evidence gives {derive_status(self.run)[0]!r}")

    def all(self) -> list:
        self.raw_results()
        self.figures()
        self.calculations()
        self.reconciliation()
        self.changes()
        self.ledger()
        self.opportunities()
        self.past_savings()
        self.gaps()
        self.status()
        return self.failures


def validate(run_dir: Path) -> list:
    """Every reason this run can't become a report. An empty list means it can."""
    run_dir = Path(run_dir)
    try:
        run = load_run(run_dir)
        return _Checker(run_dir, run).all()
    except RecordError as exc:
        return [Failure("RECORD_INVALID", "run.json", str(exc))]
    except (AttributeError, KeyError, TypeError, ValueError, InvalidOperation) as exc:
        return [Failure("RECORD_INVALID", "run.json", f"malformed record: {exc!r}")]


def print_failures(failures: list) -> None:
    for failure in failures:
        print(f"FAIL {failure.code} {failure.where}: {failure.message}")


def main(argv: list | None = None) -> int:
    parser = argparse.ArgumentParser(description="Prove a finops run record against its raw results.")
    parser.add_argument("run_dir", type=Path)
    args = parser.parse_args(argv)
    failures = validate(args.run_dir)
    if failures:
        print_failures(failures)
        return 1
    print("PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
