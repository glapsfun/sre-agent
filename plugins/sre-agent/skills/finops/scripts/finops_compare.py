#!/usr/bin/env python3
"""Compares a finops run with the last saved report and tracks whether savings landed.

The last report supplies earlier estimates and states, as evidence. Current figures
are always re-queried, so a late billing correction shows up as a difference rather
than being hidden by an old number.
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import date
from decimal import Decimal
from pathlib import Path

from finops_calc import compute, measure_window, too_early
from finops_record import RecordError, add_evidence, emit_external, gap, load_run, merge, update_scope
from finops_validate import RefError, input_values, resolve

EXIT_USAGE = 2
STATUS_TOO_EARLY = "too early to measure"
STATUS_UNMEASURED = "not yet measured"
_RUN_NAME = re.compile(r"^(\d{4}-\d{2}-\d{2})-(.+)$")
_OPPORTUNITY_ID = re.compile(r"^O-(\d+)$")
_CALC_ID = re.compile(r"^C(\d+)$")


def _matching(pattern: str, label: str):
    compiled = re.compile(pattern)

    def check(value: str) -> str:
        if not compiled.match(value):
            raise argparse.ArgumentTypeError(f"invalid {label}: {value!r}")
        return value

    return check


def _same_scope(candidate: Path, provider: str, billing_scope: str) -> bool:
    match = _RUN_NAME.match(candidate.name)
    if not match or not match.group(2).startswith(f"{provider}-{billing_scope}-"):
        return False
    try:
        scope = load_run(candidate)["scope"]
    except RecordError:
        # An unreadable record is still the last report, and carry says it couldn't be used.
        return True
    return scope.get("provider") == provider and scope.get("billing_scope") == billing_scope


def find_previous(base_dir: Path, run_dir: Path) -> Path | None:
    """The newest earlier run with the same provider and billing scope."""
    base, run_dir = Path(base_dir), Path(run_dir)
    scope = load_run(run_dir)["scope"]
    current = _RUN_NAME.match(run_dir.name)
    if not base.is_dir() or not current:
        return None
    key = (current.group(1), run_dir.name)
    earlier = [
        item
        for item in base.iterdir()
        if item.is_dir() and item.name != run_dir.name and _same_scope(item, scope["provider"], scope["billing_scope"]) and (_RUN_NAME.match(item.name).group(1), item.name) < key
    ]
    if not earlier:
        return None
    return max(earlier, key=lambda item: (_RUN_NAME.match(item.name).group(1), item.name))


def _prior_location(prior: dict, ref: str) -> list:
    """Where a reference's value sits inside the saved record."""
    match = re.match(r"^(C\d+)\.(\w+)$", ref)
    if not match:
        return ["figures", ref, "value"]
    index = next(position for position, calc in enumerate(prior.get("calculations", [])) if calc.get("id") == match.group(1))
    return ["calculations", index, "outputs", match.group(2)]


def _prior_figure(prior: dict, ref: str, evidence_id: str, basis: str) -> dict:
    found = resolve(prior, ref)
    return {
        "value": found.value,
        "kind": "estimated",
        "currency": found.currency,
        "basis": basis,
        "evidence": evidence_id,
        "locator": {"type": "json", "path": _prior_location(prior, ref)},
    }


def _next_id(prior: dict) -> str:
    numbers = [0]
    for item in [*prior.get("opportunities", []), *prior.get("past_savings", [])]:
        match = _OPPORTUNITY_ID.match(str(item.get("id", "")))
        if match:
            numbers.append(int(match.group(1)))
    return f"O-{max(numbers) + 1}"


def cmd_carry(args) -> int:
    previous = find_previous(args.base, args.run_dir)
    if previous is None:
        print("NO PREVIOUS RUN")
        return 0
    try:
        prior = load_run(previous)
    except RecordError:
        gap(f"the last report's record at {previous.name} can't be read, so the comparison is skipped", args.run_dir)
        return 0

    evidence_id = add_evidence(
        args.run_dir,
        kind="prior_run",
        source="Last report's run record",
        command=f"finops_compare.py carry, copied from {previous.name}/run.json",
        raw=(previous / "run.json").read_bytes(),
        ext="json",
    )
    figures = {}
    lines = []
    for item in prior.get("opportunities", []):
        ref = item.get("monthly")
        if not ref:
            continue
        figures[f"prior_{item['id']}_estimate"] = _prior_figure(prior, ref, evidence_id, "avoidable")
        lines.append(f"OPPORTUNITY {item['id']} state={item.get('state', 'identified')} {item.get('action', '')}")
    # A saving that was too early to measure last time stays tracked until it can be.
    for item in prior.get("past_savings", []):
        if item.get("result") is None and item.get("implemented_on"):
            figures[f"prior_{item['id']}_estimate"] = _prior_figure(prior, item["estimated"], evidence_id, "avoidable")
            lines.append(f"OPPORTUNITY {item['id']} state=implemented implemented_on={item['implemented_on']} {item.get('action', '')}")

    merge(args.run_dir, {"figures": figures})
    update_scope(args.run_dir, last_report=str(previous / "report.md"))
    print(f"PREVIOUS {previous}")
    if lines:
        emit_external("opportunities from the last report", "\n".join(lines))
    print(f"NEXT_ID {_next_id(prior)}")
    return 0


def _latest_prior(run_dir: Path, run: dict) -> tuple:
    entries = [item for item in run.get("evidence", []) if item.get("kind") == "prior_run"]
    if not entries:
        return None, None
    entry = entries[-1]
    try:
        return entry, json.loads((run_dir / entry["raw_path"]).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return entry, None


def cmd_mark(args) -> int:
    run = load_run(args.run_dir)
    estimated = f"prior_{args.id}_estimate"
    if estimated not in run.get("figures", {}):
        raise RecordError(f"{args.id} isn't in the last report. Run carry first")
    if args.state in {"implemented", "verified"} and not args.on:
        raise RecordError(f"--state {args.state} needs --on YYYY-MM-DD")

    _, prior = _latest_prior(args.run_dir, run)
    known = [*(prior or {}).get("opportunities", []), *(prior or {}).get("past_savings", [])]
    action = next((item.get("action", "") for item in known if item.get("id") == args.id), "")

    entry = {"id": args.id, "action": action, "estimated": estimated, "implemented_on": args.on, "result": None, "avoidance": None}
    if args.state == "rejected":
        entry["status"] = "rejected"
    elif args.state == "approved":
        entry["status"] = "approved, not implemented"
    else:
        data_through = run["scope"].get("data_through")
        if not data_through:
            raise RecordError("set data_through first: finops_record.py set-scope")
        implemented = date.fromisoformat(args.on)
        if too_early(implemented, date.fromisoformat(data_through)):
            entry["status"] = STATUS_TOO_EARLY
        else:
            entry["status"] = STATUS_UNMEASURED
            baseline, post = measure_window(implemented)
            print(f"WINDOW baseline={baseline} post={post}")
    merge(args.run_dir, {"past_savings": [entry]})
    print(f"MARKED {args.id} {entry['status']}")
    return 0


def _next_calc_id(run: dict) -> str:
    numbers = [0]
    for calc in run.get("calculations", []):
        match = _CALC_ID.match(str(calc.get("id", "")))
        if match:
            numbers.append(int(match.group(1)))
    return f"C{max(numbers) + 1}"


def cmd_restate(args) -> int:
    run = load_run(args.run_dir)
    entry, prior = _latest_prior(args.run_dir, run)
    if entry is None:
        print("NO PREVIOUS RUN")
        return 0
    if prior is None:
        gap("the last report's saved record can't be read, so restated totals can't be checked", args.run_dir)
        return 0

    prior_totals = {item.get("period"): item.get("query_total") for item in prior.get("reconciliation", []) if item.get("query_total")}
    for current in run.get("reconciliation", []):
        period, ref = current.get("period"), current.get("query_total")
        if not ref or period not in prior_totals:
            continue
        try:
            before = resolve(prior, prior_totals[period]).value
            after = resolve(run, ref).value
        except RefError:
            continue
        if Decimal(before) == Decimal(after):
            continue
        name = f"prior_query_total_{period}"
        run["figures"][name] = _prior_figure(prior, prior_totals[period], entry["id"], "billed")
        calc = {"id": _next_calc_id(run), "name": "change", "kind": "estimated", "assumptions": [], "inputs": {"before": name, "after": ref}}
        calc["outputs"] = compute("change", input_values(run, calc))
        run["calculations"].append(calc)
        merge(
            args.run_dir,
            {
                "figures": {name: run["figures"][name]},
                "calculations": [calc],
                "gaps": [{"text": f"The total for {period} differs from the last report, usually because of late billing data", "ref": f"{calc['id']}.change"}],
            },
        )
        print(f"RESTATED {period} {calc['id']}")
    return 0


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Compare a finops run with the last saved report. Exit 0 includes a GAP: line, 2 is a usage error.")
    commands = parser.add_subparsers(dest="command", required=True)

    carry = commands.add_parser("carry", help="bring the last report's estimates in as evidence")
    carry.add_argument("run_dir", type=Path)
    carry.add_argument("--base", type=Path, default=Path("docs/finops"))
    carry.set_defaults(handler=cmd_carry)

    mark = commands.add_parser("mark", help="record what happened to an earlier opportunity")
    mark.add_argument("run_dir", type=Path)
    mark.add_argument("--id", type=_matching(r"^O-\d+$", "opportunity ID"), required=True)
    mark.add_argument("--state", choices=["approved", "implemented", "verified", "rejected"], required=True)
    mark.add_argument("--on", type=_matching(r"^\d{4}-\d{2}-\d{2}$", "date, expected YYYY-MM-DD"))
    mark.set_defaults(handler=cmd_mark)

    restate = commands.add_parser("restate", help="flag totals that changed since the last report")
    restate.add_argument("run_dir", type=Path)
    restate.set_defaults(handler=cmd_restate)
    return parser


def main(argv: list | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        load_run(args.run_dir)
        return args.handler(args)
    except RecordError as exc:
        print(str(exc), file=sys.stderr)
        return EXIT_USAGE


if __name__ == "__main__":
    sys.exit(main())
