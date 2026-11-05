#!/usr/bin/env python3
"""Runs the shipped finops SQL on synthetic fixtures in the DuckDB CLI.

Stubbed CLIs return canned rows whatever the SQL says, so this is the only place
the query logic itself is exercised before a live trial. It proves logic, not
provider schemas or engine behavior.
"""
from __future__ import annotations

import csv
import io
import re
import subprocess
import sys
from dataclasses import dataclass, field
from decimal import Decimal, InvalidOperation
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
SQL_DIR = REPO / "plugins/sre-agent/skills/finops/sql"
FIXTURES = REPO / "tests/fixtures/finops"

# Named so the difference between shipped and tested SQL stays small and visible.
REWRITES = {
    "backticks": lambda sql: sql.replace("`", '"'),
    # DuckDB's bare NUMERIC has scale 3; BigQuery's has scale 9.
    "numeric": lambda sql: re.sub(r"\bAS NUMERIC\b", "AS DECIMAL(38, 9)", sql),
    # Athena's element_at returns a map's value; DuckDB's returns a one-item list.
    "format_timestamp": lambda sql: re.sub(r"FORMAT_TIMESTAMP\('%Y-%m', (\w+)\)", r"strftime(\1, '%Y-%m')", sql),
    "element_at": lambda sql: re.sub(r"element_at\((\w+), ('[^']*')\)", r"\1[\2]", sql),
}

GCP_BILLING_COLUMNS = {
    "billing_account_id": "VARCHAR",
    "currency": "VARCHAR",
    "cost_type": "VARCHAR",
    "invoice": "STRUCT(month VARCHAR)",
    "usage_start_time": "TIMESTAMP",
    "usage_end_time": "TIMESTAMP",
    "export_time": "TIMESTAMP",
    "service": "STRUCT(id VARCHAR, description VARCHAR)",
    "sku": "STRUCT(id VARCHAR, description VARCHAR)",
    "location": "STRUCT(location VARCHAR)",
    "project": "STRUCT(id VARCHAR, name VARCHAR)",
    "usage": "STRUCT(amount DOUBLE, unit VARCHAR, amount_in_pricing_units DOUBLE, pricing_unit VARCHAR)",
    "cost": "DOUBLE",
    "credits": "STRUCT(name VARCHAR, amount DOUBLE, type VARCHAR)[]",
    "labels": "STRUCT(key VARCHAR, value VARCHAR)[]",
}

AWS_CUR_COLUMNS = {
    "bill_payer_account_id": "VARCHAR",
    "line_item_usage_account_id": "VARCHAR",
    "line_item_line_item_type": "VARCHAR",
    "line_item_product_code": "VARCHAR",
    "line_item_usage_type": "VARCHAR",
    "line_item_usage_amount": "DOUBLE",
    "line_item_currency_code": "VARCHAR",
    "line_item_unblended_cost": "DOUBLE",
    "line_item_usage_end_date": "TIMESTAMP",
    "savings_plan_savings_plan_effective_cost": "DOUBLE",
    "savings_plan_total_commitment_to_date": "DOUBLE",
    "savings_plan_used_commitment": "DOUBLE",
    "reservation_effective_cost": "DOUBLE",
    "reservation_unused_amortized_upfront_fee_for_billing_period": "DOUBLE",
    "reservation_unused_recurring_fee": "DOUBLE",
    "reservation_reservation_a_r_n": "VARCHAR",
    "split_line_item_split_cost": "DOUBLE",
    "split_line_item_unused_cost": "DOUBLE",
}

FIXTURE_COLUMNS = {
    "gcp/billing.ndjson": GCP_BILLING_COLUMNS,
    "gcp/cud.ndjson": {
        "currency": "VARCHAR",
        "invoice": "STRUCT(month VARCHAR)",
        "service": "STRUCT(description VARCHAR)",
        "sku": "STRUCT(description VARCHAR)",
        "cost": "DOUBLE",
        "credits": "STRUCT(name VARCHAR, amount DOUBLE, type VARCHAR)[]",
    },
    "gcp/jobs.ndjson": {
        "job_id": "VARCHAR",
        "user_email": "VARCHAR",
        "job_type": "VARCHAR",
        "statement_type": "VARCHAR",
        "parent_job_id": "VARCHAR",
        "total_bytes_billed": "BIGINT",
        "total_slot_ms": "BIGINT",
        "creation_time": "TIMESTAMP",
    },
    "aws/cur2.ndjson": {**AWS_CUR_COLUMNS, "billing_period": "VARCHAR", "resource_tags": "MAP(VARCHAR, VARCHAR)"},
    "aws/cur-legacy.ndjson": {**AWS_CUR_COLUMNS, "year": "VARCHAR", "month": "VARCHAR", "bill_billing_period_start_date": "TIMESTAMP", "resource_tags_user_team": "VARCHAR"},
}


@dataclass
class Case:
    sql_file: str | Path
    params: dict[str, str]
    fixture: str
    expected_rows: list[dict[str, str]] | None
    rewrites: list[str] = field(default_factory=list)
    negative: bool = False
    # Column to expected sum across rows, for queries with many rows.
    totals: dict[str, str] = field(default_factory=dict)

    @property
    def path(self) -> Path:
        return self.sql_file if isinstance(self.sql_file, Path) else SQL_DIR / self.sql_file


GCP_OCT = {"table": "billing", "invoice_month": "2026-10"}
AWS_OCT = {"table": "cur", "period": "2026-10"}
LEGACY_OCT = {"table": "cur", "year": "2026", "month": "10"}
GCP_REWRITES = ["backticks", "numeric"]

CASES = [
    Case(
        "gcp-invoice-total.sql",
        GCP_OCT,
        "gcp/billing.ndjson",
        [{"invoice_month": "2026-10", "currency": "USD", "cost": "380.30", "credits": "-35.00", "net_cost": "345.30", "tax": "10.00"}],
        rewrites=["backticks", "numeric"],
    ),
    Case(
        FIXTURES / "gcp/naive-invoice-total.sql",
        GCP_OCT,
        "gcp/billing.ndjson",
        [{"cost": "600.00", "credits": "-50.00"}],
        negative=True,
    ),
    Case("gcp-drivers.sql", GCP_OCT, "gcp/billing.ndjson", None, rewrites=GCP_REWRITES, totals={"net_cost": "345.30", "cost": "380.30", "credits": "-35.00"}),
    Case("gcp-no-owner.sql", {**GCP_OCT, "owner_label": "team"}, "gcp/billing.ndjson", [{"invoice_month": "2026-10", "currency": "USD", "no_owner_cost": "80.30"}], rewrites=GCP_REWRITES),
    Case("gcp-effective.sql", {"table": "billing", "usage_start": "2026-10-01", "usage_end": "2026-11-01"}, "gcp/billing.ndjson", [{"usage_month": "2026-10", "currency": "USD", "effective_cost": "315.30"}], rewrites=[*GCP_REWRITES, "format_timestamp"]),
    Case(FIXTURES / "gcp/naive-usage-month.sql", {"table": "billing", "invoice_month": "2026-10"}, "gcp/billing.ndjson", [{"net_cost": "325.30"}], negative=True),
    Case("gcp-freshness.sql", GCP_OCT, "gcp/billing.ndjson", [{"max_usage_end": "2026-11-01 00:00:00", "max_export_time": "2026-11-03 06:00:00"}], rewrites=GCP_REWRITES),
    Case(
        "gcp-cud.sql",
        {"table": "billing", "first_month": "2026-08", "second_month": "2026-09"},
        "gcp/cud.ndjson",
        [
            {"invoice_month": "2026-08", "usage_cost": "100.00", "credits": "-100.00", "fee_cost": "72.00", "net_cost": "72.00"},
            {"invoice_month": "2026-09", "usage_cost": "72.00", "credits": "-72.00", "fee_cost": "72.00", "net_cost": "72.00"},
        ],
        rewrites=GCP_REWRITES,
    ),
    Case("gcp-bq-job-cost.sql", {"table": "jobs", "start_date": "2026-10-01", "end_date": "2026-11-01"}, "gcp/jobs.ndjson", [{"user_email": "ci@example.test", "total_bytes_billed": "100", "total_slot_ms": "1000"}], rewrites=GCP_REWRITES),
    Case("aws-cur2-control-total.sql", AWS_OCT, "aws/cur2.ndjson", [{"billing_period": "2026-10", "currency": "USD", "billed_cost": "770.00", "tax": "80.00"}]),
    Case("aws-cur2-effective.sql", AWS_OCT, "aws/cur2.ndjson", [{"billing_period": "2026-10", "currency": "USD", "effective_cost": "203.00"}]),
    Case("aws-cur2-effective.sql", {"table": "cur", "period": "2026-08"}, "aws/cur2.ndjson", [{"billing_period": "2026-08", "currency": "USD", "effective_cost": "5.00"}]),
    Case("aws-cur2-no-owner.sql", {**AWS_OCT, "owner_tag": "user_team"}, "aws/cur2.ndjson", [{"billing_period": "2026-10", "currency": "USD", "no_owner_cost": "620.00"}], rewrites=["element_at"]),
    Case("aws-cur2-commitments.sql", AWS_OCT, "aws/cur2.ndjson", [{"billing_period": "2026-10", "total_commitment": "20.00", "used_commitment": "15.00", "unused_commitment": "5.00"}]),
    Case("aws-cur2-freshness.sql", AWS_OCT, "aws/cur2.ndjson", [{"billing_period": "2026-10", "max_usage_end": "2026-11-01 00:00:00"}]),
    Case("aws-cur2-drivers.sql", AWS_OCT, "aws/cur2.ndjson", None, totals={"billed_cost": "770.00"}),
    # A credit stays on its own row, or it would read as a lower rate for the usage.
    Case(
        "aws-cur2-drivers.sql",
        {"table": "cur", "period": "2026-07"},
        "aws/cur2.ndjson",
        [
            {"usage_type": "BoxUsage", "line_item_type": "Usage", "quantity": "1000", "billed_cost": "1000.00"},
            {"usage_type": "BoxUsage", "line_item_type": "Credit", "quantity": "0", "billed_cost": "-200.00"},
        ],
    ),
    Case(FIXTURES / "aws/naive-effective.sql", AWS_OCT, "aws/cur2.ndjson", [{"effective_cost": "850.00"}], negative=True),
    Case(FIXTURES / "aws/naive-split.sql", AWS_OCT, "aws/cur2.ndjson", [{"billed_cost": "890.00"}], negative=True),
    Case("aws-cur-legacy-control-total.sql", LEGACY_OCT, "aws/cur-legacy.ndjson", [{"billing_period": "2026-10", "currency": "USD", "billed_cost": "770.00", "tax": "80.00"}]),
    Case("aws-cur-legacy-drivers.sql", LEGACY_OCT, "aws/cur-legacy.ndjson", None, totals={"billed_cost": "770.00"}),
]


def load_statement(table: str, fixture: str) -> str:
    columns = ", ".join(f"'{name}': '{kind}'" for name, kind in FIXTURE_COLUMNS[fixture].items())
    path = FIXTURES / fixture
    return (
        f"CREATE TABLE {table} AS SELECT * FROM read_json('{path}', "
        f"format='newline_delimited', columns={{{columns}}});"
    )


def prepared(case: Case) -> str:
    sql = case.path.read_text(encoding="utf-8")
    for name in case.rewrites:
        sql = REWRITES[name](sql)
    for key, value in case.params.items():
        sql = sql.replace("{{" + key + "}}", value)
    leftover = re.findall(r"\{\{\w+\}\}", sql)
    if leftover:
        raise ValueError(f"unfilled parameters: {leftover}")
    return sql


def same(want: str, got: str) -> bool:
    try:
        return Decimal(want) == Decimal(got)
    except InvalidOperation:
        return want == got


def run_case(case: Case) -> str | None:
    """Returns a failure message, or None when the case holds."""
    if not case.path.exists():
        return f"missing file {case.path}"
    table = case.params.get("table", "billing")
    statement = load_statement(table, case.fixture) + prepared(case)
    result = subprocess.run(
        ["duckdb", "-no-agent", "-csv", "-c", statement],
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        return f"duckdb failed: {result.stderr.strip()}"
    rows = list(csv.DictReader(io.StringIO(result.stdout)))
    for column, value in case.totals.items():
        total = sum((Decimal(row[column]) for row in rows), Decimal(0))
        if total != Decimal(value):
            return f"{column} sums to {total}, expected {value}"
    if case.expected_rows is None:
        return None
    if len(rows) != len(case.expected_rows):
        return f"expected {len(case.expected_rows)} rows, got {len(rows)}: {rows}"
    for want, got in zip(case.expected_rows, rows):
        for column, value in want.items():
            if column not in got:
                return f"missing column {column}: {got}"
            if not same(value, got[column]):
                return f"{column}: expected {value}, got {got[column]}"
    return None


def text_rules() -> list[str]:
    """Every shipped query names its columns and stays read-only and bounded."""
    sys.path.insert(0, str(SQL_DIR.parent / "scripts"))
    from finops_guard import GuardError, check_bounded, check_read_only

    problems = []
    for path in sorted(SQL_DIR.glob("*.sql")):
        raw = path.read_text(encoding="utf-8")
        sql = re.sub(r"\{\{\w+\}\}", "x", raw)
        partition = "year" if "legacy" in path.name else "billing_period" if path.name.startswith("aws-") else None
        try:
            check_read_only(sql)
            check_bounded(sql, partition)
        except GuardError as exc:
            problems.append(f"{path.name}: {exc}")
    return problems


def main() -> int:
    failures = 0
    for problem in text_rules():
        failures += 1
        print(f"FAIL: {problem}")
    for case in CASES:
        message = run_case(case)
        label = case.path.name + (" (negative control)" if case.negative else "")
        if message:
            failures += 1
            print(f"FAIL: {label}: {message}")
        else:
            print(f"ok: {label}")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
