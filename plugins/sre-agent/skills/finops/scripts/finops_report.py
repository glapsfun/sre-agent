#!/usr/bin/env python3
"""Renders report.md from a validated finops run record, proof first.

Every number comes from resolve(), so the report can only print values the
validator proved. Text from tags, labels, and resource names is untrusted and is
escaped so it can't break a table or add a heading.
"""
from __future__ import annotations

import argparse
import calendar
import os
import re
import sys
import tempfile
from decimal import ROUND_HALF_UP, Decimal
from pathlib import Path

from finops_calc import UNDEFINED, display_number, fill_formula
from finops_record import RecordError, load_run, sanitize
from finops_validate import (
    STATUS_RECONCILED,
    Resolved,
    derive_status,
    input_values,
    print_failures,
    resolve,
    validate,
)

_MONTH = re.compile(r"^(\d{4})-(\d{2})$")
_PERCENT_OUTPUTS = {"pct", "savings_pct", "residual_pct"}
_HIDDEN_OUTPUTS = {"normalized"}
_LEFT = ":---"
_RIGHT = "---:"


def fmt_money(value: str, currency: str | None, signed: bool = False, places: int = 2) -> str:
    if value == UNDEFINED:
        return UNDEFINED
    if currency is None:
        return display_number(value)
    number = Decimal(value).quantize(Decimal(1).scaleb(-places), rounding=ROUND_HALF_UP)
    amount = format(abs(number), ",f")
    sign = ""
    if number < 0:
        sign = "-"
    elif signed and number > 0:
        sign = "+"
    return f"{sign}${amount}" if currency == "USD" else f"{sign}{amount} {currency}"


def fmt_pct(value: str) -> str:
    if value == UNDEFINED:
        return UNDEFINED
    return f"{format(Decimal(value).quantize(Decimal('0.1'), rounding=ROUND_HALF_UP), 'f')}%"


def fmt_bytes(count: int) -> str:
    gigabytes = Decimal(count) / (1024**3)
    if count and gigabytes < Decimal("0.1"):
        unit, amount = "MB", Decimal(count) / (1024**2)
    else:
        unit, amount = "GB", gigabytes
    text = format(amount.quantize(Decimal("0.1"), rounding=ROUND_HALF_UP), "f")
    return f"{text[:-2] if text.endswith('.0') else text} {unit}"


def cell(text: object) -> str:
    """Makes untrusted text safe in a table cell or on one line."""
    clean = sanitize(str(text))
    clean = " ".join(clean.split())
    clean = clean.replace("\\", "\\\\").replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    for character in ("|", "`", "[", "]"):
        clean = clean.replace(character, f"\\{character}")
    return clean


def _row(cells: list) -> str:
    return "|" + "|".join(f" {item} " if item else " " for item in cells) + "|"


def _table(headers: list, aligns: list, rows: list) -> list:
    return [_row(headers), _row(aligns), *[_row(row) for row in rows]]


def _unique(items: list) -> str:
    return ", ".join(dict.fromkeys(item for item in items if item))


def _month_name(period: str | None) -> str:
    match = _MONTH.match(period or "")
    if not match:
        return (period or "").replace("_", " to ")
    return calendar.month_name[int(match.group(2))]


def _period_label(period: str | None) -> str:
    match = _MONTH.match(period or "")
    return f"{_month_name(period)} {match.group(1)}" if match else _month_name(period)


class _Report:
    def __init__(self, run: dict):
        self.run = run
        self.scope = run["scope"]

    def money(self, ref: str | None, signed: bool = False, missing: str = "not available") -> str:
        if ref is None:
            return missing
        found = resolve(self.run, ref)
        return fmt_money(found.value, found.currency, signed=signed)

    def tag(self, ref: str | None) -> str:
        return resolve(self.run, ref).tag if ref else ""

    def header(self) -> list:
        status, reason = derive_status(self.run)
        detail = f"tolerance {self.scope['tolerance_pct']}%" if status == STATUS_RECONCILED else reason
        lines = [
            f"# FinOps report: {cell(self.scope['provider'].upper())} {cell(self.scope['billing_scope'])}, {cell(_period_label(self.scope['period']))}",
            "",
            f"Question: {cell(self.scope['question'])}",
            f"Status: {status} ({cell(detail)})",
        ]
        if self.scope.get("scope_detail"):
            lines.append(f"Scope: {cell(self.scope['scope_detail'])}")
        basis = f"{self.basis_text()}, {cell(self.scope['currency'])}. Data through {cell(self.scope.get('data_through'))}."
        if self.scope.get("comparison_period"):
            basis += f" Compared with {cell(_period_label(self.scope['comparison_period']))}."
        lines.append(basis)
        if self.scope.get("last_report"):
            lines.append(f"Last report: {cell(self.scope['last_report'])}")
        lines.append(f"Query use: {self.query_use()}")
        return lines

    def basis_text(self) -> str:
        bases = {figure.get("basis") for figure in self.run.get("figures", {}).values() if figure.get("currency")}
        named = [name.capitalize() for name in ("billed", "effective") if name in bases]
        if not named:
            return "No billed figures"
        return f"{named[0]} basis" if len(named) == 1 else f"{' and '.join([named[0], named[1].lower()])} bases"

    def query_use(self) -> str:
        limits = self.run["budget"]["limits"]
        used = self.run["budget"]["used"]
        parts = []
        if limits["athena_bytes"] or used["athena_bytes"]:
            parts.append(f"Athena {fmt_bytes(used['athena_bytes'])} of {fmt_bytes(limits['athena_bytes'])} approved")
        if limits["bigquery_bytes"] or used["bigquery_bytes"]:
            parts.append(f"BigQuery {fmt_bytes(used['bigquery_bytes'])} of {fmt_bytes(limits['bigquery_bytes'])} approved")
        if limits["ce_requests"] or used["ce_requests"]:
            parts.append(f"Cost Explorer {used['ce_requests']} of {limits['ce_requests']} requests")
        return f"{', '.join(parts)}." if parts else "none."

    def reconciliation(self) -> list:
        entries = self.run.get("reconciliation", [])
        lines = ["## Reconciliation", ""]
        if not entries:
            lines.append("No reconciliation was done.")
            return lines

        rows = []

        def add(label: str, field: str, percent: bool = False, share_field: str | None = None) -> None:
            refs = [entry.get(field) for entry in entries]
            if not any(refs):
                return
            cells = []
            tags = []
            for entry, ref in zip(entries, refs):
                if ref is None:
                    cells.append("not available")
                    continue
                found: Resolved = resolve(self.run, ref)
                tags.append(found.tag)
                text = fmt_pct(found.value) if percent else fmt_money(found.value, found.currency)
                share = entry.get(share_field) if share_field else None
                if share:
                    text += f" ({fmt_pct(resolve(self.run, share).value)})"
                cells.append(text)
            if share_field:
                tags.extend(self.tag(entry.get(share_field)) for entry in entries)
            rows.append([label, *cells, _unique(tags)])

        add("Invoice before tax", "invoice")
        add("Second billing view", "second_view")
        add("Query total", "query_total")
        add("Residual", "residual")
        add("No owner tag", "no_owner", share_field="no_owner_share")
        add("Effective total", "effective_total")
        add("Billed minus effective", "billed_effective_difference")
        add("Whole-bill total", "whole_bill_total")
        add("Share of whole bill", "scope_share", percent=True)

        headers = ["Item", *[cell(_month_name(entry.get("period"))) for entry in entries], "Source"]
        aligns = [_LEFT, *[_RIGHT for _ in entries], _LEFT]
        lines.extend(_table(headers, aligns, rows))
        lines.extend(self.ledger())
        return lines

    def ledger_unreconciled(self) -> bool:
        ledger = self.run.get("kubernetes_ledger")
        if not ledger:
            return False
        value = resolve(self.run, f"{ledger['calc']}.residual_pct").value
        if value == UNDEFINED:
            return True
        return abs(Decimal(value)) > Decimal(self.scope["ledger_tolerance_pct"])

    def ledger(self) -> list:
        ledger = self.run.get("kubernetes_ledger")
        if not ledger:
            return []
        heading = "### Kubernetes ledger (unreconciled)" if self.ledger_unreconciled() else "### Kubernetes ledger"
        rows = []
        for row in ledger.get("rows", []):
            amount = self.money(row.get("amount"))
            if row.get("distributed"):
                amount += ", distributed, not counted"
            rows.append([cell(row.get("name", "")), cell(str(row.get("kind", "")).replace("_", " ")), amount, self.tag(row.get("amount"))])
        residual = f"{ledger['calc']}.residual"
        rows.append(["Residual", "", self.money(residual), self.tag(residual)])
        rows.append(["Covered charges", "", self.money(ledger.get("covered")), self.tag(ledger.get("covered"))])
        return ["", heading, "", *_table(["Item", "Kind", "Amount", "Source"], [_LEFT, _LEFT, _RIGHT, _LEFT], rows)]

    def gaps(self) -> list:
        lines = []
        for item in self.run.get("gaps", []):
            if isinstance(item, dict):
                found = resolve(self.run, item["ref"])
                amount = fmt_money(found.value, found.currency)
                lines.append(f"- {cell(item.get('text', ''))}: {amount} [{found.tag}]")
            else:
                lines.append(f"- {cell(item)}")
        if self.ledger_unreconciled():
            lines.append(f"- The Kubernetes ledger residual is above {cell(self.scope['ledger_tolerance_pct'])}% of covered charges, so workload figures aren't reconciled.")
        return ["## Gaps", "", *lines] if lines else []

    def evidence(self) -> list:
        rows = [
            [entry["id"], cell(entry.get("source", "")), cell(entry.get("query_file", "")), cell(entry.get("job_id", "")), cell(entry.get("used", "")), cell(entry.get("raw_path", ""))]
            for entry in self.run.get("evidence", [])
        ]
        lines = ["## Evidence", ""]
        if rows:
            lines.extend(_table(["ID", "Source", "Query", "Job", "Used", "Result"], [_LEFT] * 6, rows))
        else:
            lines.append("No evidence was collected.")
        provided = self.run.get("provided", [])
        if provided:
            table = [
                [entry["id"], cell(entry.get("description", "")), fmt_money(entry["value"], entry.get("currency")), cell(entry.get("given_by", "")), cell(entry.get("given_at", ""))]
                for entry in provided
            ]
            lines.extend(["", "### Provided", "", *_table(["ID", "What", "Amount", "Given by", "When"], [_LEFT, _LEFT, _RIGHT, _LEFT, _LEFT], table)])
        return lines

    def calc_result(self, calc: dict) -> str:
        outputs = calc.get("outputs", {})
        if calc.get("name") == "change":
            found = resolve(self.run, f"{calc['id']}.change")
            return f"{fmt_money(found.value, found.currency)} ({fmt_pct(outputs['pct'])})"
        shown = []
        for name, value in outputs.items():
            if name in _HIDDEN_OUTPUTS:
                continue
            found = resolve(self.run, f"{calc['id']}.{name}")
            if name in _PERCENT_OUTPUTS:
                shown.append((name, fmt_pct(value)))
            elif name == "months":
                shown.append((name, f"{display_number(value)} months" if value != UNDEFINED else UNDEFINED))
            elif name == "unit_cost":
                shown.append((name, fmt_money(value, found.currency, places=4)))
            else:
                shown.append((name, fmt_money(value, found.currency)))
        if len(shown) == 1:
            return shown[0][1]
        return ", ".join(f"{name.replace('_', ' ')} {text}" for name, text in shown)

    def calculations(self) -> list:
        calculations = self.run.get("calculations", [])
        if not calculations:
            return []
        rows = []
        for calc in calculations:
            tag = f"{calc['id']} est" if calc.get("kind") == "estimated" else calc["id"]
            formula = fill_formula(calc["name"], input_values(self.run, calc))
            assumptions = list(calc.get("assumptions", []))
            rows.append([tag, cell(formula), self.calc_result(calc), cell("; ".join(assumptions))])
        return ["## Calculations", "", *_table(["ID", "Formula", "Result", "Assumes"], [_LEFT] * 4, rows)]

    def cause_text(self, driver: dict) -> str:
        cause = driver.get("cause")
        if cause is None:
            return "" if driver.get("name") == "Other" else "Not established"
        text = cell(cause.get("text", ""))
        by_id = {entry["id"]: entry for entry in self.run.get("evidence", [])}
        reported = [by_id[item]["source"] for item in cause.get("evidence", []) if by_id.get(item, {}).get("kind") == "reported"]
        return f"{text} ({cell(', '.join(reported))})" if reported else text

    def changes(self) -> list:
        changes = self.run.get("changes")
        if not changes:
            return []
        total = resolve(self.run, changes["total"])
        heading = f"## What changed: {fmt_money(total.value, total.currency, signed=True)} ({fmt_pct(resolve(self.run, changes['pct']).value)}) [{total.tag}]"
        rows = []
        for driver in changes.get("drivers", []):
            cause = driver.get("cause") or {}
            sources = [self.tag(driver.get("change")), *cause.get("evidence", [])]
            rows.append(
                [
                    cell(driver.get("name", "")),
                    self.money(driver.get("quantity"), signed=True, missing=""),
                    self.money(driver.get("rate"), signed=True, missing=""),
                    self.money(driver.get("change"), signed=True),
                    self.cause_text(driver),
                    _unique(sources),
                ]
            )
        headers = ["Driver", "Quantity", "Rate", "Change", "Cause", "Source"]
        return [heading, "", *_table(headers, [_LEFT, _RIGHT, _RIGHT, _RIGHT, _LEFT, _LEFT], rows)]

    def past_savings(self) -> list:
        items = self.run.get("past_savings", [])
        if not items:
            return []
        with_avoidance = any(item.get("avoidance") for item in items)
        headers = ["ID", "Action", "Estimated/mo", "Result/mo"] + (["Cost avoidance"] if with_avoidance else []) + ["Status", "Source"]
        aligns = [_LEFT, _LEFT, _RIGHT, _RIGHT] + ([_RIGHT] if with_avoidance else []) + [_LEFT, _LEFT]
        rows = []
        for item in items:
            row = [cell(item.get("id", "")), cell(item.get("action", "")), self.money(item.get("estimated")), self.money(item.get("result"), missing="not measured")]
            if with_avoidance:
                row.append(self.money(item.get("avoidance"), missing=""))
            row.extend(
                [
                    cell(item.get("status", "")),
                    _unique([self.tag(item.get("estimated")), self.tag(item.get("result")), self.tag(item.get("avoidance"))]),
                ]
            )
            rows.append(row)
        return ["## Did past savings land?", "", *_table(headers, aligns, rows)]

    def opportunities(self) -> list:
        items = self.run.get("opportunities", [])
        if not items:
            return []
        total_ref = self.run.get("savings_total")
        heading = "## What to do"
        if total_ref:
            total = resolve(self.run, total_ref)
            suffix = " estimated" if total.estimated else ""
            heading = f"## What to do: {fmt_money(total.value, total.currency)}/mo{suffix} [{total.tag}]"
        rows = []
        for item in items:
            reliability = item.get("reliability") or {}
            check = _unique([cell(reliability.get("text", "")), *reliability.get("evidence", [])])
            if item.get("needs_investigation"):
                check = f"Needs investigation, not in the total. {check}".strip()
            rows.append(
                [
                    cell(item.get("id", "")),
                    cell(item.get("action", "")),
                    cell(item.get("owner") or "no owner"),
                    self.money(item.get("monthly")),
                    cell("; ".join(item.get("assumes", []))),
                    check,
                    _unique([self.tag(item.get("monthly")), item.get("overlap_calc") or ""]),
                ]
            )
        headers = ["ID", "Action", "Owner", "Est./mo", "Assumes", "Reliability check", "Source"]
        return [heading, "", *_table(headers, [_LEFT, _LEFT, _LEFT, _RIGHT, _LEFT, _LEFT, _LEFT], rows)]

    def render(self) -> str:
        sections = [
            self.header(),
            self.reconciliation(),
            self.gaps(),
            self.evidence(),
            self.calculations(),
            self.changes(),
            self.past_savings(),
            self.opportunities(),
        ]
        blocks = ["\n".join(section) for section in sections if section]
        return "\n\n".join(blocks) + "\n"


def render(run: dict) -> str:
    return _Report(run).render()


def _write_atomically(path: Path, text: str) -> None:
    handle, temp_name = tempfile.mkstemp(dir=path.parent, prefix=".report-", suffix=".tmp")
    try:
        with os.fdopen(handle, "w", encoding="utf-8") as temp:
            temp.write(text)
        os.replace(temp_name, path)
    except BaseException:
        Path(temp_name).unlink(missing_ok=True)
        raise


def main(argv: list | None = None) -> int:
    parser = argparse.ArgumentParser(description="Validate a finops run and render its report.md. A run that fails validation gets no report.")
    parser.add_argument("run_dir", type=Path)
    args = parser.parse_args(argv)
    path = args.run_dir / "report.md"

    failures = validate(args.run_dir)
    if failures:
        # A report left over from an earlier pass would look proved when it isn't.
        path.unlink(missing_ok=True)
        print_failures(failures)
        return 1
    try:
        run = load_run(args.run_dir)
    except RecordError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    _write_atomically(path, render(run))
    print(f"REPORT {path}")
    print(f"STATUS {derive_status(run)[0]}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
