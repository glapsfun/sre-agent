#!/usr/bin/env python3
"""Budget-guarded, read-only GCP billing queries for finops runs.

These scripts write evidence entries themselves and the model never does, which
is what lets the validator prove each measured figure later.
"""
from __future__ import annotations

import argparse
import csv
import io
import json
import re
import sys
import uuid
from pathlib import Path

from finops_guard import (
    BudgetError,
    GuardError,
    add_pending,
    check_bounded,
    check_read_only,
    clear_pending,
    default_basis,
    load_query,
    pending_queries,
    require_budget,
    settle_query,
    strip_leading_comments,
)
from finops_record import RecordError, add_evidence, add_gap, emit_external, empty_result_gap, failure_reason, gap, load_run, parse_params, run_cli, show_result
from finops_report import fmt_bytes

EXIT_USAGE = 2
EXIT_BUDGET = 3
TIMED_OUT = 124
# bq returns only 100 rows by default, which would silently undercount a bill.
MAX_ROWS = 1000000
MONITORING_URL = "https://monitoring.googleapis.com/v3/projects/{project}/timeSeries"
MONTH_PARAMS = ("invoice_month", "first_month", "second_month")
_DRY_RUN_TEXT = re.compile(r"process (\d+) bytes")
_BYTES_LIMIT = re.compile(r"exceeded limit for bytes billed", re.IGNORECASE)
_MONTH = re.compile(r"^\d{4}-\d{2}$")


def _matching(pattern: str, label: str):
    compiled = re.compile(pattern)

    def check(value: str) -> str:
        if not compiled.match(value):
            raise argparse.ArgumentTypeError(f"invalid {label}: {value!r}")
        return value

    return check


_project = _matching(r"^[A-Za-z0-9:._-]+$", "project")
_location = _matching(r"^[A-Za-z0-9-]+$", "location")
_table = _matching(r"^[A-Za-z0-9_.:-]+$", "table")
_recommender = _matching(r"^[A-Za-z0-9_.]+$", "recommender")
_time = _matching(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$", "time, expected YYYY-MM-DDTHH:MM:SSZ")
_filter = _matching(r"^[^\x00-\x1f]+$", "filter")


def _budget_stop(message: object) -> int:
    print(f"BUDGET: {message}")
    return EXIT_BUDGET


def _identity() -> str:
    code, out, _ = run_cli(["gcloud", "auth", "list", "--filter=status:ACTIVE", "--format=value(account)"])
    return out.strip() if code == 0 else ""


def _bq(args, *global_flags: str) -> list:
    return ["bq", "--quiet", f"--project_id={args.project}", f"--location={args.location}", *global_flags]


def _prepared(args) -> str:
    params = parse_params(args.param)
    for key in MONTH_PARAMS:
        if key in params and not _MONTH.match(params[key]):
            raise GuardError(f"{key} must be YYYY-MM like the run's periods, got {params[key]!r}. The prepared queries convert it to the export's YYYYMM")
    sql = load_query(args.query_file, params)
    check_read_only(sql)
    check_bounded(sql, None)
    return sql


def _dry_run(args, sql: str) -> int | None:
    """Bytes a query would process, without running or billing it. None on failure."""
    argv = _bq(args, "--format=json") + ["query", "--use_legacy_sql=false", "--dry_run", strip_leading_comments(sql)]
    code, out, err = run_cli(argv)
    if code:
        gap(failure_reason(f"bq dry run of {args.query_file.name}", code, f"{err}\n{out}"), args.run_dir)
        return None
    try:
        return int(json.loads(out)["statistics"]["query"]["totalBytesProcessed"])
    except (ValueError, KeyError, TypeError):
        pass
    text = _DRY_RUN_TEXT.search(out)
    if text:
        return int(text.group(1))
    gap(f"bq dry run of {args.query_file.name} returned output that couldn't be read, so its cost is unknown and it wasn't run", args.run_dir)
    return None


def cmd_estimate(args) -> int:
    sql = _prepared(args)
    estimate = _dry_run(args, sql)
    if estimate is not None:
        print(f"BYTES {estimate} {fmt_bytes(estimate)}")
    return 0


def cmd_schema(args) -> int:
    code, out, err = run_cli(_bq(args, "--format=json") + ["show", "--schema", args.table])
    if code:
        gap(failure_reason(f"bq show --schema {args.table}", code, f"{err}\n{out}"), args.run_dir)
        return 0
    emit_external(f"schema of {args.table}", out)
    return 0


def _job_statistics(args, job_id: str, estimate: int) -> tuple:
    """Bytes billed and reservation for a finished job."""
    code, out, err = run_cli(_bq(args, "--format=json") + ["show", "-j", job_id])
    try:
        if code:
            raise ValueError(err)
        statistics = json.loads(out)["statistics"]
        return int(statistics["query"]["totalBytesBilled"]), statistics.get("reservation_id")
    except (ValueError, KeyError, TypeError):
        add_gap(args.run_dir, f"couldn't read bytes billed for BigQuery job {job_id}, so the budget records the dry-run estimate")
        return estimate, None


def _settle_pending(args) -> str | None:
    """Charges jobs an earlier call lost track of. Returns why a new one can't start."""
    for item in pending_queries(args.run_dir, "bigquery"):
        job = item["job_id"]
        code, out, _ = run_cli(_bq(args, "--format=json") + ["show", "-j", job])
        try:
            if code:
                raise ValueError(job)
            body = json.loads(out)
            if body.get("status", {}).get("state") != "DONE":
                return f"BigQuery job {job} is still running, so its use is unknown. Try again when it finishes"
            # A job that failed has no query statistics, and bills nothing.
            billed = int(body.get("statistics", {}).get("query", {}).get("totalBytesBilled", 0))
        except (ValueError, KeyError, TypeError, AttributeError):
            # It may never have started, but counting too much is the safe side of a budget.
            billed = int(item.get("estimate", 0))
            add_gap(args.run_dir, f"couldn't read BigQuery job {job}, so the budget records its dry-run estimate")
        settle_query(args.run_dir, "bigquery_bytes", job, billed)
    return None


def cmd_query(args) -> int:
    sql = _prepared(args)
    unsettled = _settle_pending(args)
    if unsettled:
        return _budget_stop(unsettled)
    try:
        remaining = require_budget(load_run(args.run_dir), "bigquery_bytes", 1)
    except BudgetError as exc:
        return _budget_stop(exc)

    estimate = _dry_run(args, sql)
    if estimate is None:
        return 0
    if estimate > remaining:
        return _budget_stop(f"bigquery_bytes: {args.query_file.name} would process {estimate}, {remaining} remain")

    job_id = f"finops-{uuid.uuid4().hex}"
    flags = ["--format=csv", f"--job_id={job_id}"]
    query_flags = ["--use_legacy_sql=false", f"--maximum_bytes_billed={remaining}", f"--max_rows={MAX_ROWS}"]
    add_pending(args.run_dir, "bigquery", job_id, estimate)
    code, out, err = run_cli(_bq(args, *flags) + ["query", *query_flags, strip_leading_comments(sql)])
    if code == TIMED_OUT:
        gap(f"BigQuery job {job_id} timed out here and may still be running. Its use isn't counted yet, and the next query checks it first. Don't start it again", args.run_dir)
        return 0
    if code:
        details = f"{err}\n{out}"
        if _BYTES_LIMIT.search(details):
            clear_pending(args.run_dir, job_id)
            return _budget_stop(f"bigquery_bytes: {args.query_file.name} would be billed more than the {remaining} remaining, and failed without charge")
        # The client names the job, so it can exist and bill even though bq exited non-zero.
        gap(f"{failure_reason(f'bq query {args.query_file.name}', code, details)}. BigQuery job {job_id} may have started anyway. Its use isn't counted yet, and the next query checks it first", args.run_dir)
        return 0

    billed, reservation = _job_statistics(args, job_id, estimate)
    settle_query(args.run_dir, "bigquery_bytes", job_id, billed)
    if reservation:
        add_gap(args.run_dir, "this project uses capacity pricing, so bytes billed have no direct dollar meaning")

    table = list(csv.reader(io.StringIO(out)))
    rows = max(len(table) - 1, 0)
    truncated = rows >= MAX_ROWS
    params = parse_params(args.param)
    evidence_id = add_evidence(
        args.run_dir,
        kind="billing",
        source=args.source or f"BigQuery, {args.query_file.stem}",
        command=f"bq --project_id={args.project} --location={args.location} query --job_id={job_id}",
        raw=out.encode(),
        ext="csv",
        query_text=sql,
        identity=_identity(),
        job_id=job_id,
        used=fmt_bytes(billed),
        basis=args.basis or default_basis(args.query_file),
        complete=not truncated,
        rows=rows,
        role="query",
        period=params.get("invoice_month", ""),
    )
    show_result(args.run_dir, evidence_id, args.query_file.name, out)
    if truncated:
        gap(f"{args.query_file.name} reached {MAX_ROWS} rows and may be truncated, so it can't back a figure", args.run_dir)
    else:
        empty_result_gap(", ".join(filter(None, [args.query_file.name, params.get("invoice_month", "")])), table or [[]], args.run_dir)

    try:
        require_budget(load_run(args.run_dir), "bigquery_bytes", 0)
    except BudgetError as exc:
        return _budget_stop(exc)
    return 0


def cmd_recommendations(args) -> int:
    argv = ["gcloud", "recommender", "recommendations", "list", "--project", args.project, "--location", args.location, "--recommender", args.recommender, "--format=json"]
    code, out, err = run_cli(argv)
    if code:
        gap(failure_reason("gcloud recommender recommendations list", code, err), args.run_dir)
        return 0
    evidence_id = add_evidence(
        args.run_dir,
        kind="recommendation",
        source=f"GCP Recommender, {args.recommender}",
        command=" ".join(argv),
        raw=out.encode(),
        ext="json",
        identity=_identity(),
        used="1 call",
        basis="avoidable",
        rows=len(json.loads(out)),
    )
    show_result(args.run_dir, evidence_id, args.recommender, out)
    return 0


def cmd_monitoring(args) -> int:
    code, token, err = run_cli(["gcloud", "auth", "print-access-token"])
    if code or not token.strip():
        gap(failure_reason("gcloud auth print-access-token", code or 1, err), args.run_dir)
        return 0
    # The token goes through stdin, so it can't appear in a process listing or evidence.
    config = f'header = "Authorization: Bearer {token.strip()}"\n'
    url = MONITORING_URL.format(project=args.project)
    params = [f"filter={args.filter}", f"interval.startTime={args.start}", f"interval.endTime={args.end}"]

    pages: list = []
    complete = True
    page_token = None
    while True:
        query = [*params, *([f"pageToken={page_token}"] if page_token else [])]
        argv = ["curl", "-sS", "--fail", "-G", "--config", "-", url]
        for item in query:
            argv += ["--data-urlencode", item]
        code, out, err = run_cli(argv, stdin=config)
        if code:
            gap(failure_reason("Cloud Monitoring timeSeries", code, err), args.run_dir)
            complete = False
            break
        pages.append(out.strip())
        page_token = json.loads(out).get("nextPageToken")
        if not page_token:
            break
    if not pages:
        return 0

    raw = '{"pages": [' + ",".join(pages) + "]}"
    evidence_id = add_evidence(
        args.run_dir,
        kind="telemetry",
        source=f"Cloud Monitoring, {args.project}",
        command=f"curl -G {url} filter={args.filter} interval.startTime={args.start} interval.endTime={args.end}",
        raw=raw.encode(),
        ext="json",
        identity=_identity(),
        used=f"{len(pages)} request{'s' if len(pages) != 1 else ''}",
        complete=complete,
    )
    show_result(args.run_dir, evidence_id, f"Cloud Monitoring, {args.project}", raw)
    return 0


def _parser() -> argparse.ArgumentParser:
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("run_dir", type=Path)
    common.add_argument("--project", type=_project, required=True)

    located = argparse.ArgumentParser(add_help=False)
    located.add_argument("--location", type=_location, required=True, help="dataset or recommender location")

    query_flags = argparse.ArgumentParser(add_help=False)
    query_flags.add_argument("--query-file", type=Path, required=True)
    query_flags.add_argument("--param", action="append", default=[], metavar="KEY=VALUE")

    parser = argparse.ArgumentParser(description="Budget-guarded, read-only GCP billing queries. Exit 0 includes a GAP: line, 2 is a usage error or refused query, 3 is a budget stop.")
    commands = parser.add_subparsers(dest="command", required=True)

    estimate = commands.add_parser("estimate", parents=[common, located, query_flags], help="bytes a query would process, without running it")
    estimate.set_defaults(handler=cmd_estimate)

    schema = commands.add_parser("schema", parents=[common, located], help="a table's columns")
    schema.add_argument("--table", type=_table, required=True)
    schema.set_defaults(handler=cmd_schema)

    query = commands.add_parser("query", parents=[common, located, query_flags], help="run one read-only query under the approved budget")
    query.add_argument("--source", default="")
    query.add_argument("--basis", choices=["billed", "effective", "avoidable"], help="defaults to effective for a query named effective, otherwise billed")
    query.set_defaults(handler=cmd_query)

    recommendations = commands.add_parser("recommendations", parents=[common, located], help="native savings recommendations")
    recommendations.add_argument("--recommender", type=_recommender, required=True)
    recommendations.set_defaults(handler=cmd_recommendations)

    monitoring = commands.add_parser("monitoring", parents=[common], help="Cloud Monitoring time series, as telemetry evidence")
    monitoring.add_argument("--filter", type=_filter, required=True)
    monitoring.add_argument("--start", type=_time, required=True)
    monitoring.add_argument("--end", type=_time, required=True)
    monitoring.set_defaults(handler=cmd_monitoring)
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
        gap(f"{args.command}: the CLI returned unreadable output, so nothing was recorded", args.run_dir)
        return 0


if __name__ == "__main__":
    sys.exit(main())
