"""Read-only SQL checks, query loading, and the run's query budget.

These checks guard against mistakes, and aren't a SQL parser. Athena and BigQuery
permissions are still what keep a run from changing anything. The partition check
proves a filter names the column, not that it prunes: `<>` or `OR` would still pass.
"""
from __future__ import annotations

import re
from pathlib import Path

from finops_record import load_run, updating

BUDGET_KINDS = {"athena_bytes", "bigquery_bytes", "ce_requests"}

_FORBIDDEN = re.compile(
    r"\b(CREATE|INSERT|UPDATE|DELETE|MERGE|DROP|ALTER|TRUNCATE|UNLOAD|GRANT|REVOKE|CALL|EXPORT|EXECUTE|DECLARE|BEGIN|LOAD)\b",
    re.IGNORECASE,
)
_STAR = re.compile(r"\bSELECT\s+(DISTINCT\s+)?\*|,\s*\*|\w\.\*", re.IGNORECASE)
_PLACEHOLDER = re.compile(r"\{\{(\w+)\}\}")
_SAFE_VALUE = re.compile(r"^[A-Za-z0-9_.:-]+$")
_TOKEN = re.compile(r"''|[\w$]+(?:\.[\w$]+)*|[(),]")
_CLAUSE_ENDS = {"WHERE", "GROUP", "HAVING", "ORDER", "LIMIT", "OFFSET", "FETCH", "WINDOW", "QUALIFY", "UNION", "EXCEPT", "INTERSECT"}
_NOT_TABLES = {"UNNEST", "LATERAL"}


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


def _nested(bare: str) -> list:
    """Upper-case words and commas, with each pair of parentheses as its own list."""
    root: list = []
    stack = [root]
    for token in _TOKEN.findall(bare.upper()):
        if token == "(":
            stack.append([])
            stack[-2].append(stack[-1])
        elif token == ")":
            if len(stack) > 1:
                stack.pop()
        else:
            stack[-1].append(token)
    return root


def _is_query(item: object) -> bool:
    return isinstance(item, list) and bool(item) and item[0] in {"SELECT", "WITH"}


def _clause(block: list, keyword: str) -> list:
    """What follows a keyword in one SELECT block, up to its next clause."""
    if keyword not in block:
        return []
    rest = block[block.index(keyword) + 1 :]
    end = next((index for index, item in enumerate(rest) if isinstance(item, str) and item in _CLAUSE_ENDS), len(rest))
    return rest[:end]


def _common_table_names(group: list) -> set:
    names: set = set()
    for index, item in enumerate(group):
        if isinstance(item, list):
            names |= _common_table_names(item)
        elif item == "AS" and index and isinstance(group[index - 1], str) and _is_query(group[index + 1] if index + 1 < len(group) else None):
            names.add(group[index - 1])
    return names


def _reads_table(block: list, common_tables: set) -> bool:
    """Whether a block's FROM or JOIN reaches anything other than another query block."""
    expecting_source = True
    for item in _clause(block, "FROM"):
        if expecting_source and not (_is_query(item) or (isinstance(item, str) and (item in _NOT_TABLES or item in common_tables))):
            return True
        expecting_source = item in (",", "JOIN")
    return False


def _names_tested(where: list) -> set:
    """Column names in a WHERE clause, leaving out any subquery, which filters only itself."""
    names: set = set()
    for item in where:
        if isinstance(item, str):
            names.add(item.rsplit(".", 1)[-1])
        elif not _is_query(item):
            names |= _names_tested(item)
    return names


def _has_unfiltered_read(group: list, partition: str, common_tables: set) -> bool:
    starts = [index for index, item in enumerate(group) if item == "SELECT"]
    for start, end in zip(starts, [*starts[1:], len(group)]):
        block = group[start:end]
        if _reads_table(block, common_tables) and partition not in _names_tested(_clause(block, "WHERE")):
            return True
    return any(_has_unfiltered_read(item, partition, common_tables) for item in group if isinstance(item, list))


def check_bounded(sql: str, partition_column: str | None) -> None:
    """Named columns always. When the engine bills by scan, also a partition filter in the
    WHERE of every block that reads a table, since one elsewhere doesn't bound that read."""
    bare = _strip_comments_and_strings(sql)
    if _STAR.search(bare):
        raise GuardError("name the columns: SELECT * scans every column")
    if partition_column is None:
        return
    tree = _nested(bare)
    if _has_unfiltered_read(tree, partition_column.upper(), _common_table_names(tree)):
        raise GuardError(f"filter on the partition column {partition_column} in the WHERE of every block that reads a table, or the query scans every period")


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


def default_basis(query_file: Path) -> str:
    """A query's basis when none is asked for, so an effective query can't record billed by omission."""
    return "effective" if "effective" in Path(query_file).stem else "billed"


def strip_leading_comments(sql: str) -> str:
    """Removes comments before the statement. A CLI could read text starting with
    dashes as a flag, so this is what gets sent. Evidence keeps the full query."""
    text = sql.lstrip()
    while True:
        if text.startswith("--") or text.startswith("#"):
            _, _, text = text.partition("\n")
        elif text.startswith("/*"):
            _, _, text = text.partition("*/")
        else:
            return text
        text = text.lstrip()


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
    with updating(run_dir) as run:
        run["budget"]["used"][kind] += amount


def add_pending(run_dir: Path, engine: str, job_id: str, estimate: int = 0) -> None:
    """Notes a started query, so its cost still counts if this script loses track of it."""
    with updating(run_dir) as run:
        run["budget"].setdefault("pending", []).append({"engine": engine, "job_id": job_id, "estimate": estimate})


def clear_pending(run_dir: Path, job_id: str) -> None:
    """Stops tracking a query that is known to have cost nothing."""
    with updating(run_dir) as run:
        run["budget"]["pending"] = [item for item in run["budget"].get("pending", []) if item.get("job_id") != job_id]


def settle_query(run_dir: Path, kind: str, job_id: str, amount: int) -> None:
    """Charges a started query and stops tracking it, as one step. Two overlapping calls
    can both see it finish, and only the first one that gets here charges it."""
    if kind not in BUDGET_KINDS:
        raise BudgetError(f"unknown budget kind: {kind}")
    with updating(run_dir) as run:
        pending = run["budget"].get("pending", [])
        remaining = [item for item in pending if item.get("job_id") != job_id]
        if len(remaining) != len(pending):
            run["budget"]["used"][kind] += amount
            run["budget"]["pending"] = remaining


def pending_queries(run_dir: Path, engine: str) -> list:
    return [item for item in load_run(run_dir)["budget"].get("pending", []) if item.get("engine") == engine]
