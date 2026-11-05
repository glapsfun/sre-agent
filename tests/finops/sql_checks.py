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
    "usage": "STRUCT(amount DOUBLE, unit VARCHAR)",
    "cost": "DOUBLE",
    "credits": "STRUCT(name VARCHAR, amount DOUBLE, type VARCHAR)[]",
    "labels": "STRUCT(key VARCHAR, value VARCHAR)[]",
}

FIXTURE_COLUMNS = {
    "gcp/billing.ndjson": GCP_BILLING_COLUMNS,
}


@dataclass
class Case:
    sql_file: str | Path
    params: dict[str, str]
    fixture: str
    expected_rows: list[dict[str, str]]
    rewrites: list[str] = field(default_factory=list)
    negative: bool = False

    @property
    def path(self) -> Path:
        return self.sql_file if isinstance(self.sql_file, Path) else SQL_DIR / self.sql_file


GCP_OCT = {"table": "billing", "invoice_month": "2026-10"}

CASES = [
    Case(
        "gcp-invoice-total.sql",
        GCP_OCT,
        "gcp/billing.ndjson",
        [{"invoice_month": "2026-10", "currency": "USD", "cost": "390.30", "credits": "-35.00", "net_cost": "355.30"}],
        rewrites=["backticks", "numeric"],
    ),
    Case(
        FIXTURES / "gcp/naive-invoice-total.sql",
        GCP_OCT,
        "gcp/billing.ndjson",
        [{"cost": "600.00", "credits": "-50.00"}],
        negative=True,
    ),
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
    if len(rows) != len(case.expected_rows):
        return f"expected {len(case.expected_rows)} rows, got {len(rows)}: {rows}"
    for want, got in zip(case.expected_rows, rows):
        for column, value in want.items():
            if column not in got:
                return f"missing column {column}: {got}"
            if not same(value, got[column]):
                return f"{column}: expected {value}, got {got[column]}"
    return None


def main() -> int:
    failures = 0
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
