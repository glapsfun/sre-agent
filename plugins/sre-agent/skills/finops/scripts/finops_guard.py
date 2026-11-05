"""Read-only SQL checks, query loading, and the run's query budget.

The keyword check is a guard against mistakes, not a SQL parser. Athena and
BigQuery permissions are still what keep a run from changing anything.
"""
from __future__ import annotations

import re
from pathlib import Path

from finops_record import load_run, save_run

BUDGET_KINDS = {"athena_bytes", "bigquery_bytes", "ce_requests"}

_FORBIDDEN = re.compile(
    r"\b(CREATE|INSERT|UPDATE|DELETE|MERGE|DROP|ALTER|TRUNCATE|UNLOAD|GRANT|REVOKE|CALL|EXPORT|EXECUTE|DECLARE|BEGIN|LOAD)\b",
    re.IGNORECASE,
)
_STAR = re.compile(r"\bSELECT\s+(DISTINCT\s+)?\*|,\s*\*|\w\.\*", re.IGNORECASE)
_PLACEHOLDER = re.compile(r"\{\{(\w+)\}\}")
_SAFE_VALUE = re.compile(r"^[A-Za-z0-9_.:-]+$")


class GuardError(Exception):
    """A query that must not run. Runners turn this into exit 2, before any cloud call."""


class BudgetError(Exception):
    """A query the approved budget doesn't cover. Runners turn this into exit 3."""


def _strip_comments_and_strings(sql: str) -> str:
    """Blanks out what can't be a statement, so keywords inside it don't count."""
    out = []
    index = 0
    size = len(sql)
    while index < size:
        character = sql[index]
        pair = sql[index : index + 2]
        if pair == "--" or character == "#":
            end = sql.find("\n", index)
            index = size if end == -1 else end
        elif pair == "/*":
            end = sql.find("*/", index + 2)
            if end == -1:
                raise GuardError("unterminated comment")
            index = end + 2
        elif character in "'\"`":
            index = _skip_quoted(sql, index, character)
            out.append("''")
        else:
            out.append(character)
            index += 1
    return "".join(out)


def _skip_quoted(sql: str, start: int, quote: str) -> int:
    index = start + 1
    while index < len(sql):
        if sql[index] == "\\" and quote != "`":
            index += 2
        elif sql[index] == quote:
            if sql[index + 1 : index + 2] == quote:
                index += 2
            else:
                return index + 1
        else:
            index += 1
    raise GuardError("unterminated quoted text")


def check_read_only(sql: str) -> None:
    """Allows exactly one statement, and only if it reads."""
    bare = _strip_comments_and_strings(sql)
    statements = [part.strip() for part in bare.split(";") if part.strip()]
    if not statements:
        raise GuardError("the query is empty")
    if len(statements) > 1:
        raise GuardError("only one statement can run at a time")
    first = statements[0].split(None, 1)[0].upper()
    if first not in {"SELECT", "WITH"}:
        raise GuardError(f"only SELECT or WITH can run, not {first}")
    found = _FORBIDDEN.search(statements[0])
    if found:
        raise GuardError(f"{found.group(1).upper()} can't run: finops queries are read-only")


def check_bounded(sql: str, partition_column: str | None) -> None:
    """Named columns always, and a partition filter when the engine bills by scan."""
    bare = _strip_comments_and_strings(sql)
    if _STAR.search(bare):
        raise GuardError("name the columns: SELECT * scans every column")
    if partition_column is None:
        return
    where = re.search(r"\bWHERE\b(.*)", bare, re.IGNORECASE | re.DOTALL)
    if where is None or not re.search(rf"\b{re.escape(partition_column)}\b", where.group(1), re.IGNORECASE):
        raise GuardError(f"filter on the partition column {partition_column}, or the query scans every period")


def load_query(path: Path, params: dict) -> str:
    """Fills a prepared query's {{name}} placeholders with strictly checked values."""
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as exc:
        raise GuardError(f"can't read {path}: {exc}") from exc
    names = set(_PLACEHOLDER.findall(text))
    if names - set(params):
        raise GuardError(f"missing parameters: {sorted(names - set(params))}")
    if set(params) - names:
        raise GuardError(f"unknown parameters: {sorted(set(params) - names)}")
    for key, value in params.items():
        if not _SAFE_VALUE.match(value):
            raise GuardError(f"unsafe value for {key}: {value!r}")
    return _PLACEHOLDER.sub(lambda match: params[match.group(1)], text)


def require_budget(run: dict, kind: str, amount: int = 0) -> int:
    """Returns what remains before this amount. Raises BudgetError if it doesn't fit."""
    if kind not in BUDGET_KINDS:
        raise BudgetError(f"unknown budget kind: {kind}")
    budget = run["budget"]
    if not budget.get("approved"):
        raise BudgetError("the budget hasn't been approved")
    limit = budget["limits"][kind]
    used = budget["used"][kind]
    if used + amount > limit:
        raise BudgetError(f"{kind}: limit {limit}, used {used}, needed {amount}")
    return limit - used


def record_use(run_dir: Path, kind: str, amount: int) -> None:
    if kind not in BUDGET_KINDS:
        raise BudgetError(f"unknown budget kind: {kind}")
    run = load_run(run_dir)
    run["budget"]["used"][kind] += amount
    save_run(run_dir, run)
