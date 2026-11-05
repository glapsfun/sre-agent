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

from finops_guard import (
    BudgetError,
    GuardError,
    add_pending,
    check_bounded,
    check_read_only,
    clear_pending,
    load_query,
    pending_queries,
    record_use,
    require_budget,
    strip_leading_comments,
)
from finops_record import RecordError, add_evidence, emit_external, empty_result_gap, failure_reason, gap, load_run, parse_params, run_cli, show_result
from finops_report import fmt_bytes

BILLING_REGION = "us-east-1"
DEFAULT_CATALOG = "AwsDataCatalog"
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


def _month_of(start: str, end: str) -> str:
    """The billing period a date range covers exactly, or nothing if it isn't one month."""
    year, month = int(start[:4]), int(start[5:7])
    following = f"{year + (month == 12):04d}-{month % 12 + 1:02d}-01"
    return start[:7] if start.endswith("-01") and end == following else ""


def _query_period(params: dict) -> str:
    if params.get("period"):
        return params["period"]
    if params.get("year") and params.get("month", "").isdigit():
        return f"{params['year']}-{int(params['month']):02d}"
    return ""


def _budget_stop(message: object) -> int:
    print(f"BUDGET: {message}")
    return EXIT_BUDGET


def cmd_invoice(args) -> int:
    scope = load_run(args.run_dir)["scope"]
    if scope.get("scope_kind") == "whole-bill" and args.account != scope.get("billing_scope"):
        raise RecordError(f"this run covers {scope.get('billing_scope')}, so {args.account}'s invoices can't reconcile it")
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

    body = json.loads(out)
    summaries = body.get("InvoiceSummaries", [])
    complete = not body.get("NextToken")
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
        complete=complete,
        role="invoice",
        period=args.period,
        currencies=currencies,
    )
    lines = []
    for index, item in enumerate(summaries):
        amount = item.get("BaseCurrencyAmount", {})
        entity = item.get("Entity", {}).get("InvoicingEntity", "unknown")
        path = ["InvoiceSummaries", index, "BaseCurrencyAmount", "TotalAmountBeforeTax"]
        lines.append(f"INVOICE {index} {entity} {item.get('BillType', '')} {amount.get('TotalAmountBeforeTax', '')} {amount.get('CurrencyCode', '')} path={json.dumps(path)}")
    show_result(args.run_dir, evidence_id, f"invoices for {args.period}", "\n".join(lines))
    if not complete:
        gap(f"invoices for {args.period}: the list is incomplete, so it can't back a figure", args.run_dir)
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


def cmd_tables(args) -> int:
    if args.database:
        label = f"tables in {args.database}"
        argv = _aws(args, "athena", "list-table-metadata", "--catalog-name", args.catalog, "--database-name", args.database)
    else:
        label = f"databases in {args.catalog}"
        argv = _aws(args, "athena", "list-databases", "--catalog-name", args.catalog)
    code, out, err = run_cli(argv)
    if code:
        gap(failure_reason(f"aws athena {label}", code, err), args.run_dir)
        return 0
    body = json.loads(out)
    if args.database:
        lines = [f"TABLE {item.get('Name')}" for item in body.get("TableMetadataList", [])]
    else:
        lines = [f"DATABASE {item.get('Name')}" for item in body.get("DatabaseList", [])]
    emit_external(label, "\n".join(lines))
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
            gap(f"Athena query {query_id} started, but its status couldn't be read. Its use isn't counted yet, and the next query checks it first. Don't start it again", args.run_dir)
            return None
        execution = json.loads(out).get("QueryExecution", {})
        state = execution.get("Status", {}).get("State", "")
        if state in TERMINAL_STATES:
            return execution
        if time.monotonic() >= deadline:
            gap(f"Athena query {query_id} is still {state} after {limit:g} seconds. Its use isn't counted yet, and the next query checks it first. Don't start it again", args.run_dir)
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


def _settle_pending(args) -> str | None:
    """Charges queries an earlier call lost track of. Returns why a new one can't start."""
    for item in pending_queries(args.run_dir, "athena"):
        job = item["job_id"]
        code, out, _ = run_cli(_aws(args, "athena", "get-query-execution", "--query-execution-id", job))
        if code:
            return f"Athena query {job} can't be checked, so its use is unknown. Try again once it can be"
        execution = json.loads(out).get("QueryExecution", {})
        state = execution.get("Status", {}).get("State", "")
        if state not in TERMINAL_STATES:
            return f"Athena query {job} is still {state}, so its use is unknown. Try again when it finishes"
        record_use(args.run_dir, "athena_bytes", int(execution.get("Statistics", {}).get("DataScannedInBytes", 0)))
        clear_pending(args.run_dir, job)
    return None


def cmd_athena(args) -> int:
    params = parse_params(args.param)
    sql = load_query(args.query_file, params)
    check_read_only(sql)
    check_bounded(sql, args.partition_column)
    unsettled = _settle_pending(args)
    if unsettled:
        return _budget_stop(unsettled)
    run = load_run(args.run_dir)
    try:
        require_budget(run, "athena_bytes", 1)
    except BudgetError as exc:
        return _budget_stop(exc)

    label = f"aws athena start-query-execution {args.query_file.name}"
    start = _aws(args, "athena", "start-query-execution", "--work-group", args.workgroup, "--query-execution-context", f"Database={args.database}", "--query-string", strip_leading_comments(sql))
    code, out, err = run_cli(start)
    if code:
        gap(failure_reason(label, code, err), args.run_dir)
        return 0
    query_id = json.loads(out)["QueryExecutionId"]
    add_pending(args.run_dir, "athena", query_id)

    execution = _wait_for_query(args, query_id)
    if execution is None:
        return 0
    scanned = int(execution.get("Statistics", {}).get("DataScannedInBytes", 0))
    # Failed and cancelled queries are still charged for what they scanned.
    record_use(args.run_dir, "athena_bytes", scanned)
    clear_pending(args.run_dir, query_id)
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
        role="query",
        period=_query_period(params),
    )
    show_result(args.run_dir, evidence_id, args.query_file.name, text)
    if complete:
        empty_result_gap(", ".join(filter(None, [args.query_file.name, _query_period(params)])), rows, args.run_dir)

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


def _save_pages(args, base: list, source: str, pages: list, complete: bool, basis: str = "", period: str = "") -> None:
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
        role="query",
        period=period,
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
    _save_pages(args, base, f"Cost Explorer {args.metric}, {args.start} to {args.end}", pages, complete, basis=args.basis, period=_month_of(args.start, args.end))
    return code


def cmd_ce_commitment(args) -> int:
    base = ["ce", args.op, "--time-period", f"Start={args.start},End={args.end}"]
    pages, complete, code = _paged_cost_explorer(args, base, COMMITMENT_OPS[args.op])
    _save_pages(args, base, f"Cost Explorer {args.op}, {args.start} to {args.end}", pages, complete)
    return code


def _hub_recommendations(args) -> int:
    # Left deduplicated by resource on purpose: competing recommendations for one
    # resource would otherwise look like savings that add up.
    argv = _aws(args, "cost-optimization-hub", "list-recommendations", region=BILLING_REGION)
    code, out, err = run_cli(argv)
    if code:
        gap(f"{failure_reason('aws cost-optimization-hub list-recommendations', code, err)}. It may not be enrolled; try --source compute-optimizer", args.run_dir)
        return 0
    body = json.loads(out)
    complete = not body.get("nextToken")
    evidence_id = add_evidence(
        args.run_dir,
        kind="recommendation",
        source="Cost Optimization Hub",
        command=" ".join(argv),
        raw=out.encode(),
        ext="json",
        identity=args.profile or "",
        used="1 call",
        basis="avoidable",
        complete=complete,
        rows=len(body.get("items", [])),
    )
    show_result(args.run_dir, evidence_id, "Cost Optimization Hub", out)
    if not complete:
        gap("Cost Optimization Hub: the list is incomplete, so it can't back a figure", args.run_dir)
    return 0


def _compute_optimizer_recommendations(args) -> int:
    # This operation has no CLI paginator, so it returns one page and a token.
    pages: list = []
    rows = 0
    token = None
    complete = True
    while True:
        command = ["compute-optimizer", "get-ec2-instance-recommendations"]
        if token:
            command += ["--next-token", token]
        code, out, err = run_cli(_aws(args, *command))
        if code:
            gap(failure_reason("aws compute-optimizer get-ec2-instance-recommendations", code, err), args.run_dir)
            complete = False
            break
        pages.append(out.strip())
        body = json.loads(out)
        rows += len(body.get("instanceRecommendations", []))
        token = body.get("nextToken")
        if not token:
            break
    if not pages:
        return 0
    raw = '{"pages": [' + ",".join(pages) + "]}"
    evidence_id = add_evidence(
        args.run_dir,
        kind="recommendation",
        source="Compute Optimizer",
        command="aws compute-optimizer get-ec2-instance-recommendations",
        raw=raw.encode(),
        ext="json",
        identity=args.profile or "",
        used=f"{len(pages)} call{'s' if len(pages) != 1 else ''}",
        basis="avoidable",
        complete=complete,
        rows=rows,
    )
    show_result(args.run_dir, evidence_id, "Compute Optimizer", raw)
    return 0


def cmd_recommendations(args) -> int:
    return _hub_recommendations(args) if args.source == "hub" else _compute_optimizer_recommendations(args)


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

    tables = commands.add_parser("tables", parents=[common], help="databases, or a database's tables, to find a CUR table")
    tables.add_argument("--catalog", type=_name, default=DEFAULT_CATALOG)
    tables.add_argument("--database", type=_name)
    tables.set_defaults(handler=cmd_tables)

    schema = commands.add_parser("schema", parents=[common], help="a table's columns and partitions")
    schema.add_argument("--catalog", type=_name, default=DEFAULT_CATALOG)
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
