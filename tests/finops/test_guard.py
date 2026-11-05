from __future__ import annotations

import re
import tempfile
import unittest
from datetime import date
from pathlib import Path

from support import SCRIPTS

from finops_guard import BudgetError, GuardError, check_bounded, check_read_only, load_query, record_use, require_budget
from finops_record import init_run, load_run, save_run

SQL_DIR = SCRIPTS.parent / "sql"


class ReadOnly(unittest.TestCase):
    def test_reads_pass(self):
        for sql in (
            "SELECT 1",
            "select 1;",
            "WITH a AS (SELECT 1) SELECT * FROM a",
            "SELECT 'DROP TABLE' AS note -- DROP",
            "SELECT /* DELETE FROM x */ 1",
            "SELECT \"UPDATE\" FROM t # INSERT",
            "SELECT REPLACE(name, 'a', 'b') FROM t",
            "SELECT line_item_update_time FROM `proj.ds.delete_me`",
            "SELECT 'it''s; fine; DROP'",
        ):
            with self.subTest(sql=sql):
                check_read_only(sql)

    def test_writes_refused(self):
        for sql in (
            "SELECT 1; DROP TABLE x",
            "SELECT 1; SELECT 2",
            "CREATE TABLE x AS SELECT 1",
            "CREATE OR REPLACE TABLE x AS SELECT 1",
            "INSERT INTO x VALUES (1)",
            "UNLOAD (SELECT 1) TO 's3://x'",
            "MERGE INTO x USING y ON a = b",
            "EXPORT DATA OPTIONS() AS SELECT 1",
            "CALL p()",
            "DELETE FROM x",
            "UPDATE x SET y=1",
            "DROP TABLE x",
            "ALTER TABLE x ADD COLUMN y INT",
            "TRUNCATE TABLE x",
            "GRANT SELECT ON x TO y",
            "WITH a AS (SELECT 1) INSERT INTO x SELECT * FROM a",
            "select 1;\ndrop table x",
            "DECLARE x INT64; SELECT x",
            "LOAD DATA INTO x FROM FILES()",
        ):
            with self.subTest(sql=sql), self.assertRaises(GuardError):
                check_read_only(sql)

    def test_empty_and_unterminated_refused(self):
        for sql in ("", "   ", "-- only a comment", "SELECT 'unterminated"):
            with self.subTest(sql=sql), self.assertRaises(GuardError):
                check_read_only(sql)


class Bounded(unittest.TestCase):
    def test_star_refused(self):
        for sql in ("SELECT * FROM t WHERE billing_period='2026-10'", "SELECT t.* FROM t WHERE billing_period='2026-10'", "SELECT a, * FROM t WHERE billing_period='2026-10'"):
            with self.subTest(sql=sql), self.assertRaises(GuardError):
                check_bounded(sql, "billing_period")

    def test_star_inside_count_passes(self):
        check_bounded("SELECT COUNT(*) FROM t WHERE billing_period='2026-10'", "billing_period")
        check_bounded("SELECT COUNT(*) FROM t", None)

    def test_partition_filter_required(self):
        with self.assertRaises(GuardError):
            check_bounded("SELECT a FROM t", "billing_period")
        with self.assertRaises(GuardError):
            check_bounded("SELECT billing_period FROM t WHERE a = 1", "billing_period")
        check_bounded("SELECT a FROM t WHERE billing_period='2026-10'", "billing_period")

    def test_partition_named_only_in_a_string_does_not_count(self):
        with self.assertRaises(GuardError):
            check_bounded("SELECT a FROM t WHERE note = 'billing_period'", "billing_period")

    def test_no_partition_column_applies_only_the_star_rule(self):
        check_bounded("SELECT a FROM t", None)


class LoadQuery(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.path = Path(self._tmp.name) / "q.sql"
        self.path.write_text("SELECT a FROM `{{table}}` WHERE p = '{{period}}'\n")

    def test_substitutes(self):
        self.assertEqual(load_query(self.path, {"table": "proj.ds.t", "period": "2026-10"}), "SELECT a FROM `proj.ds.t` WHERE p = '2026-10'\n")

    def test_missing_and_extra_parameters(self):
        with self.assertRaises(GuardError):
            load_query(self.path, {"table": "t"})
        with self.assertRaises(GuardError):
            load_query(self.path, {"table": "t", "period": "x", "other": "y"})

    def test_values_can_not_break_out_of_the_query(self):
        for value in ("x'; DROP", "a b", "a`b", "a/*b", "", "a\nb"):
            with self.subTest(value=value), self.assertRaises(GuardError):
                load_query(self.path, {"table": "t", "period": value})

    def test_missing_file(self):
        with self.assertRaises(GuardError):
            load_query(self.path.with_name("nope.sql"), {})

    def test_every_shipped_query_is_read_only(self):
        files = sorted(SQL_DIR.glob("*.sql"))
        self.assertTrue(files)
        for path in files:
            names = set(re.findall(r"\{\{(\w+)\}\}", path.read_text(encoding="utf-8")))
            with self.subTest(path=path.name):
                check_read_only(load_query(path, {name: "x" for name in names}))


class Budget(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.run_dir = init_run(
            Path(self._tmp.name),
            provider="aws",
            billing_scope="123456789012",
            scope_kind="whole-bill",
            period="2026-10",
            comparison_period=None,
            currency="USD",
            question="q",
            today=date(2026, 11, 5),
        )

    def approve(self, limit=10, used=0):
        run = load_run(self.run_dir)
        run["budget"]["approved"] = True
        run["budget"]["limits"]["ce_requests"] = limit
        run["budget"]["used"]["ce_requests"] = used
        save_run(self.run_dir, run)
        return load_run(self.run_dir)

    def test_unapproved_is_refused(self):
        with self.assertRaises(BudgetError):
            require_budget(load_run(self.run_dir), "ce_requests")

    def test_over_the_limit_is_refused_and_says_the_numbers(self):
        with self.assertRaises(BudgetError) as caught:
            require_budget(self.approve(limit=10, used=8), "ce_requests", 3)
        for number in ("10", "8", "3"):
            self.assertIn(number, str(caught.exception))

    def test_within_the_limit_returns_what_remains(self):
        self.assertEqual(require_budget(self.approve(limit=10, used=8), "ce_requests", 2), 2)

    def test_unknown_kind_is_refused(self):
        with self.assertRaises(BudgetError):
            require_budget(self.approve(), "dollars")

    def test_record_use_persists_and_adds_up(self):
        self.approve()
        record_use(self.run_dir, "ce_requests", 2)
        record_use(self.run_dir, "ce_requests", 3)
        self.assertEqual(load_run(self.run_dir)["budget"]["used"]["ce_requests"], 5)


if __name__ == "__main__":
    unittest.main()
