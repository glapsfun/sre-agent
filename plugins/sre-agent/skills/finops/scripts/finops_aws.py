#!/usr/bin/env python3
"""Budget-guarded, read-only AWS billing queries for finops runs.

These scripts write evidence entries themselves and the model never does, which
is what lets the validator prove each measured figure later.
"""
from __future__ import annotations

import argparse
import csv
import io
import json
import os
import re
import sys
import time
from pathlib import Path

from finops_guard import BudgetError, GuardError, check_bounded, check_read_only, load_query, record_use, require_budget
from finops_record import RecordError, add_evidence, emit_external, failure_reason, gap, load_run, parse_params, run_cli, show_result
from finops_report import fmt_bytes

BILLING_REGION = "us-east-1"
EXIT_USAGE = 2
EXIT_BUDGET = 3
RESULT_PAGE_ITEMS = "1000"
TERMINAL_STATES = {"SUCCEEDED", "FAILED", "CANCELLED"}
METRICS = ["UnblendedCost", "AmortizedCost", "NetAmortizedCost", "NetUnblendedCost", "BlendedCost", "UsageQuantity"]
# The name each operation uses for its page token differs across Cost Explorer.
COMMITMENT_OPS = {
    "get-savings-plans-utilization": None,
    "get-savings-plans-coverage": ("NextToken", "--next-token"),
    "get-reservation-utilization": ("NextPageToken", "--next-page-token"),
    "get-reservation-coverage": ("NextPageToken", "--next-page-token"),
}


def _matching(pattern: str, label: str):
    compiled = re.compile(pattern)

    def check(value: str) -> str:
        if not compiled.match(value):
            raise argparse.ArgumentTypeError(f"invalid {label}: {value!r}")
        return value

    return check


_account = _matching(r"^\d{12}$", "account")
_month = _matching(r"^\d{4}-\d{2}$", "period, expected YYYY-MM")
_day = _matching(r"^\d{4}-\d{2}-\d{2}$", "date, expected YYYY-MM-DD")
_name = _matching(r"^[A-Za-z0-9_.-]+$", "name")
_group = _matching(r"^(DIMENSION|TAG|COST_CATEGORY):[A-Za-z0-9_:.-]+$", "group, expected DIMENSION:KEY")


def _aws(args, *command: str, region: str | None = None) -> list:
    argv = ["aws"]
    if args.profile:
        argv += ["--profile", args.profile]
    argv += list(command)
    if region:
        argv += ["--region", region]
    return argv + ["--output", "json"]


def _budget_stop(message: object) -> int:
    print(f"BUDGET: {message}")
    return EXIT_BUDGET


def cmd_invoice(args) -> int:
    year, month = args.period.split("-")
    label = "aws invoicing list-invoice-summaries"
    argv = _aws(
        args,
        "invoicing",
        "list-invoice-summaries",
        "--selector",
        f"ResourceType=ACCOUNT_ID,Value={args.account}",
        "--filter",
        f"BillingPeriod={{Month={int(month)},Year={year}}}",
        region=BILLING_REGION,
    )
    code, out, err = run_cli(argv)
    if code:
        gap(failure_reason(label, code, err), args.run_dir)
        return 0

    summaries = json.loads(out).get("InvoiceSummaries", [])
    currencies = sorted({item.get("BaseCurrencyAmount", {}).get("CurrencyCode", "") for item in summaries} - {""})
    evidence_id = add_evidence(
        args.run_dir,
        kind="billing",
        source=f"AWS Invoicing, {args.period}",
        command=" ".join(argv),
        raw=out.encode(),
        ext="json",
        identity=args.profile or "",
        used="1 call",
        basis="billed",
        currency=currencies[0] if len(currencies) == 1 else "",
        rows=len(summaries),
    )
    lines = []
    for index, item in enumerate(summaries):
        amount = item.get("BaseCurrencyAmount", {})
        entity = item.get("Entity", {}).get("InvoicingEntity", "unknown")
        path = ["InvoiceSummaries", index, "BaseCurrencyAmount", "TotalAmountBeforeTax"]
        lines.append(f"INVOICE {index} {entity} {item.get('BillType', '')} {amount.get('TotalAmountBeforeTax', '')} {amount.get('CurrencyCode', '')} path={json.dumps(path)}")
    show_result(args.run_dir, evidence_id, f"invoices for {args.period}", "\n".join(lines))
    if not summaries:
        gap(f"no invoice issued yet for {args.period}", args.run_dir)
    elif len(currencies) > 1:
        gap(f"invoices for {args.period} are in more than one currency ({', '.join(currencies)}), so they can't be summed", args.run_dir)
    return 0


def cmd_workgroup(args) -> int:
    label = "aws athena get-work-group"
    code, out, err = run_cli(_aws(args, "athena", "get-work-group", "--work-group", args.workgroup))
    if code:
        gap(failure_reason(label, code, err), args.run_dir)
        return 0
    configuration = json.loads(out).get("WorkGroup", {}).get("Configuration", {})
    limit = configuration.get("BytesScannedCutoffPerQuery")
    location = configuration.get("ResultConfiguration", {}).get("OutputLocation", "not set")
    lines = [f"LIMIT {limit}" if limit else "NO LIMIT: one query can overshoot the budget", f"OUTPUT {location}: query results get written here"]
    emit_external(f"workgroup {args.workgroup}", "\n".join(lines))
    return 0


def cmd_schema(args) -> int:
    label = "aws athena get-table-metadata"
    argv = _aws(args, "athena", "get-table-metadata", "--catalog-name", args.catalog, "--database-name", args.database, "--table-name", args.table)
    code, out, err = run_cli(argv)
    if code:
        gap(failure_reason(label, code, err), args.run_dir)
        return 0
    metadata = json.loads(out).get("TableMetadata", {})
    lines = [f"PARTITION {item.get('Name')} {item.get('Type')}" for item in metadata.get("PartitionKeys", [])]
    lines += [f"COLUMN {item.get('Name')} {item.get('Type')}" for item in metadata.get("Columns", [])]
    emit_external(f"schema of {args.database}.{args.table}", "\n".join(lines))
    return 0


def _wait_for_query(args, query_id: str) -> dict | None:
    poll = float(os.environ.get("FINOPS_POLL_SECONDS", "2"))
    limit = float(os.environ.get("FINOPS_POLL_LIMIT", "300"))
    deadline = time.monotonic() + limit
    while True:
        code, out, err = run_cli(_aws(args, "athena", "get-query-execution", "--query-execution-id", query_id))
        if code:
            gap(failure_reason(f"aws athena get-query-execution {query_id}", code, err), args.run_dir)
            return None
        execution = json.loads(out).get("QueryExecution", {})
        state = execution.get("Status", {}).get("State", "")
        if state in TERMINAL_STATES:
            return execution
        if time.monotonic() >= deadline:
            gap(f"Athena query {query_id} is still {state} after {limit:g} seconds. Check it and its cost in the console", args.run_dir)
            return None
        time.sleep(poll)


def _fetch_results(args, query_id: str) -> tuple:
    """Every row across every page, and whether all pages arrived."""
    rows: list = []
    token = None
    while True:
        command = ["athena", "get-query-results", "--query-execution-id", query_id, "--max-items", RESULT_PAGE_ITEMS]
        if token:
            command += ["--starting-token", token]
        code, out, err = run_cli(_aws(args, *command))
        if code:
            gap(failure_reason(f"aws athena get-query-results {query_id}", code, err), args.run_dir)
            return rows, False
        body = json.loads(out)
        for row in body.get("ResultSet", {}).get("Rows", []):
            rows.append([cell.get("VarCharValue", "") for cell in row.get("Data", [])])
        token = body.get("NextToken")
        if not token:
            return rows, True


def _to_csv(rows: list) -> str:
    buffer = io.StringIO()
    csv.writer(buffer, lineterminator="\n").writerows(rows)
    return buffer.getvalue()


def cmd_athena(args) -> int:
    sql = load_query(args.query_file, parse_params(args.param))
    check_read_only(sql)
    check_bounded(sql, args.partition_column)
    run = load_run(args.run_dir)
    try:
        require_budget(run, "athena_bytes", 1)
    except BudgetError as exc:
        return _budget_stop(exc)

    label = f"aws athena start-query-execution {args.query_file.name}"
    start = _aws(args, "athena", "start-query-execution", "--work-group", args.workgroup, "--query-execution-context", f"Database={args.database}", "--query-string", sql)
    code, out, err = run_cli(start)
    if code:
        gap(failure_reason(label, code, err), args.run_dir)
        return 0
    query_id = json.loads(out)["QueryExecutionId"]

    execution = _wait_for_query(args, query_id)
    if execution is None:
        return 0
    scanned = int(execution.get("Statistics", {}).get("DataScannedInBytes", 0))
    # Failed and cancelled queries are still charged for what they scanned.
    record_use(args.run_dir, "athena_bytes", scanned)
    status = execution.get("Status", {})
    if status.get("State") != "SUCCEEDED":
        gap(f"Athena query {query_id} {status.get('State')}: {status.get('StateChangeReason', 'no reason given')}", args.run_dir)
        return 0

    rows, complete = _fetch_results(args, query_id)
    text = _to_csv(rows)
    evidence_id = add_evidence(
        args.run_dir,
        kind="billing",
        source=args.source or f"Athena, {args.query_file.stem}",
        command=f"aws athena start-query-execution --work-group {args.workgroup} --query-execution-context Database={args.database}",
        raw=text.encode(),
        ext="csv",
        query_text=sql,
        identity=args.profile or "",
        job_id=query_id,
        used=fmt_bytes(scanned),
        basis=args.basis,
        complete=complete,
        rows=max(len(rows) - 1, 0),
    )
    show_result(args.run_dir, evidence_id, args.query_file.name, text)
    if len(rows) <= 1 and complete:
        gap("query returned no rows", args.run_dir)

    run = load_run(args.run_dir)
    try:
        require_budget(run, "athena_bytes", 0)
    except BudgetError as exc:
        return _budget_stop(exc)
    return 0


def _paged_cost_explorer(args, base: list, token_names: tuple | None) -> tuple:
    """Charges one request per page. Returns raw pages, whether all arrived, and an exit code."""
    pages: list = []
    token = None
    while True:
        try:
            require_budget(load_run(args.run_dir), "ce_requests", 1)
        except BudgetError as exc:
            return pages, False, _budget_stop(exc)
        command = list(base)
        if token:
            command += [token_names[1], token]
        code, out, err = run_cli(_aws(args, *command, region=BILLING_REGION))
        if code:
            gap(failure_reason(f"aws {' '.join(base[:2])}", code, err), args.run_dir)
            return pages, False, 0
        record_use(args.run_dir, "ce_requests", 1)
        pages.append(out)
        token = json.loads(out).get(token_names[0]) if token_names else None
        if not token:
            return pages, True, 0


def _save_pages(args, base: list, source: str, pages: list, complete: bool, basis: str = "") -> None:
    if not pages:
        return
    # Pages are joined as returned, so their numbers keep the provider's exact text.
    raw = '{"pages": [' + ",".join(page.strip() for page in pages) + "]}"
    evidence_id = add_evidence(
        args.run_dir,
        kind="billing",
        source=source,
        command=" ".join(["aws", *base, "--region", BILLING_REGION]),
        raw=raw.encode(),
        ext="json",
        identity=args.profile or "",
        used=f"{len(pages)} request{'s' if len(pages) != 1 else ''}",
        basis=basis,
        complete=complete,
    )
    show_result(args.run_dir, evidence_id, source, raw)
    if not complete:
        gap(f"{source}: results are incomplete, so they can't back a figure", args.run_dir)


def cmd_ce(args) -> int:
    base = ["ce", "get-cost-and-usage", "--time-period", f"Start={args.start},End={args.end}", "--granularity", args.granularity, "--metrics", args.metric]
    if args.group_by:
        base.append("--group-by")
        for item in args.group_by:
            kind, key = item.split(":", 1)
            base.append(f"Type={kind},Key={key}")
    pages, complete, code = _paged_cost_explorer(args, base, ("NextPageToken", "--next-page-token"))
    _save_pages(args, base, f"Cost Explorer {args.metric}, {args.start} to {args.end}", pages, complete, basis=args.basis)
    return code


def cmd_ce_commitment(args) -> int:
    base = ["ce", args.op, "--time-period", f"Start={args.start},End={args.end}"]
    pages, complete, code = _paged_cost_explorer(args, base, COMMITMENT_OPS[args.op])
    _save_pages(args, base, f"Cost Explorer {args.op}, {args.start} to {args.end}", pages, complete)
    return code


def cmd_recommendations(args) -> int:
    if args.source == "hub":
        # Left deduplicated by resource on purpose: competing recommendations for
        # one resource would otherwise look like savings that add up.
        argv = _aws(args, "cost-optimization-hub", "list-recommendations", region=BILLING_REGION)
        label, items_key, source = "aws cost-optimization-hub list-recommendations", "items", "Cost Optimization Hub"
    else:
        argv = _aws(args, "compute-optimizer", "get-ec2-instance-recommendations")
        label, items_key, source = "aws compute-optimizer get-ec2-instance-recommendations", "instanceRecommendations", "Compute Optimizer"
    code, out, err = run_cli(argv)
    if code:
        message = failure_reason(label, code, err)
        if args.source == "hub":
            message += ". It may not be enrolled; try --source compute-optimizer"
        gap(message, args.run_dir)
        return 0
    items = json.loads(out).get(items_key, [])
    evidence_id = add_evidence(
        args.run_dir,
        kind="recommendation",
        source=source,
        command=" ".join(argv),
        raw=out.encode(),
        ext="json",
        identity=args.profile or "",
        used="1 call",
        basis="avoidable",
        rows=len(items),
    )
    show_result(args.run_dir, evidence_id, source, out)
    return 0


def _parser() -> argparse.ArgumentParser:
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("run_dir", type=Path)
    common.add_argument("--profile", type=_name, help="named AWS profile, recorded in evidence")

    parser = argparse.ArgumentParser(description="Budget-guarded, read-only AWS billing queries. Exit 0 includes a GAP: line, 2 is a usage error or refused query, 3 is a budget stop.")
    commands = parser.add_subparsers(dest="command", required=True)

    invoice = commands.add_parser("invoice", parents=[common], help="invoice totals for a billing period")
    invoice.add_argument("--account", type=_account, required=True)
    invoice.add_argument("--period", type=_month, required=True)
    invoice.set_defaults(handler=cmd_invoice)

    workgroup = commands.add_parser("workgroup", parents=[common], help="a workgroup's scan limit and result location")
    workgroup.add_argument("--workgroup", type=_name, required=True)
    workgroup.set_defaults(handler=cmd_workgroup)

    schema = commands.add_parser("schema", parents=[common], help="a table's columns and partitions")
    schema.add_argument("--catalog", type=_name, required=True)
    schema.add_argument("--database", type=_name, required=True)
    schema.add_argument("--table", type=_name, required=True)
    schema.set_defaults(handler=cmd_schema)

    athena = commands.add_parser("athena", parents=[common], help="run one read-only query under the approved budget")
    athena.add_argument("--workgroup", type=_name, required=True)
    athena.add_argument("--database", type=_name, required=True)
    athena.add_argument("--query-file", type=Path, required=True)
    athena.add_argument("--param", action="append", default=[], metavar="KEY=VALUE")
    athena.add_argument("--partition-column", type=_name, default="billing_period")
    athena.add_argument("--source", default="")
    athena.add_argument("--basis", choices=["billed", "effective", "avoidable"], default="billed")
    athena.set_defaults(handler=cmd_athena)

    ce = commands.add_parser("ce", parents=[common], help="Cost Explorer costs, charged per request")
    ce.add_argument("--start", type=_day, required=True)
    ce.add_argument("--end", type=_day, required=True)
    ce.add_argument("--granularity", choices=["MONTHLY", "DAILY"], required=True)
    ce.add_argument("--metric", choices=METRICS, required=True)
    ce.add_argument("--group-by", type=_group, action="append", default=[], metavar="DIMENSION:KEY")
    ce.add_argument("--basis", choices=["billed", "effective"], default="billed")
    ce.set_defaults(handler=cmd_ce)

    commitment = commands.add_parser("ce-commitment", parents=[common], help="commitment utilization or coverage, charged per request")
    commitment.add_argument("--op", choices=sorted(COMMITMENT_OPS), required=True)
    commitment.add_argument("--start", type=_day, required=True)
    commitment.add_argument("--end", type=_day, required=True)
    commitment.set_defaults(handler=cmd_ce_commitment)

    recommendations = commands.add_parser("recommendations", parents=[common], help="native savings recommendations")
    recommendations.add_argument("--source", choices=["hub", "compute-optimizer"], default="hub")
    recommendations.set_defaults(handler=cmd_recommendations)
    return parser


def main(argv: list | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        load_run(args.run_dir)
        return args.handler(args)
    except (GuardError, RecordError) as exc:
        print(str(exc), file=sys.stderr)
        return EXIT_USAGE
    except json.JSONDecodeError:
        gap(f"aws {args.command}: the CLI returned unreadable output, so nothing was recorded", args.run_dir)
        return 0


if __name__ == "__main__":
    sys.exit(main())
