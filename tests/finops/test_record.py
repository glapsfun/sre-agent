from __future__ import annotations

import hashlib
import json
import subprocess
import tempfile
import unittest
from datetime import date
from pathlib import Path
from unittest import mock

from support import run_script

import finops_record
from finops_record import (
    LocatorError,
    RecordError,
    add_evidence,
    add_gap,
    add_provided,
    init_run,
    is_closed,
    load_run,
    locate,
    merge,
    sanitize,
    save_run,
    update_scope,
)


class RecordCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp = Path(self._tmp.name)
        self.base = self.tmp / "docs/finops"

    def new_run(self, **overrides):
        args = dict(
            provider="aws",
            billing_scope="123456789012",
            scope_kind="whole-bill",
            period="2026-10",
            comparison_period="2026-09",
            currency="USD",
            question="why did spend rise?",
            today=date(2026, 11, 5),
        )
        args.update(overrides)
        return init_run(self.base, **args)


class InitRun(RecordCase):
    def test_directory_name_and_collision(self):
        self.assertEqual(self.new_run().name, "2026-11-05-aws-123456789012-2026-10")
        self.assertEqual(self.new_run().name, "2026-11-05-aws-123456789012-2026-10-2")

    def test_defaults(self):
        run = load_run(self.new_run())
        self.assertEqual(run["scope"]["tolerance_pct"], "0.1")
        self.assertEqual(run["scope"]["ledger_tolerance_pct"], "5")
        self.assertFalse(run["budget"]["approved"])
        self.assertEqual(run["evidence"], [])

    def test_unsafe_scope_and_period_refused(self):
        with self.assertRaises(RecordError):
            self.new_run(billing_scope="../x")
        with self.assertRaises(RecordError):
            self.new_run(period="10/2026")
        with self.assertRaises(RecordError):
            self.new_run(scope_kind="most")
        with self.assertRaises(RecordError):
            self.new_run(provider="azure")

    def test_part_month_period_accepted(self):
        run_dir = self.new_run(period="2026-10-01_2026-10-07", scope_kind="partial")
        self.assertEqual(load_run(run_dir)["scope"]["period"], "2026-10-01_2026-10-07")


class Evidence(RecordCase):
    def test_ids_raw_bytes_and_hash(self):
        run_dir = self.new_run()
        raw = b"billing_period,amount\n2026-10,1.00\n"
        first = add_evidence(run_dir, kind="billing", source="Athena", command="aws athena x", raw=raw, ext="csv", query_text="SELECT 1", job_id="abc", used="2.1 GB", basis="billed", currency="USD", rows=1)
        second = add_evidence(run_dir, kind="telemetry", source="CloudWatch", command="aws cloudwatch y", raw=b"{}", ext="json")
        self.assertEqual((first, second), ("E1", "E2"))
        self.assertEqual((run_dir / "evidence/E1.csv").read_bytes(), raw)
        entry = load_run(run_dir)["evidence"][0]
        self.assertEqual(entry["sha256"], hashlib.sha256(raw).hexdigest())
        self.assertEqual(entry["raw_path"], "evidence/E1.csv")
        self.assertEqual(entry["query_file"], "evidence/E1.sql")
        self.assertEqual((run_dir / "evidence/E1.sql").read_text(), "SELECT 1")
        self.assertEqual(entry["job_id"], "abc")
        self.assertTrue(entry["complete"])

    def test_command_is_saved_when_there_is_no_query(self):
        run_dir = self.new_run()
        add_evidence(run_dir, kind="billing", source="Invoicing", command="aws invoicing list-invoice-summaries", raw=b"{}", ext="json")
        entry = load_run(run_dir)["evidence"][0]
        self.assertEqual(entry["query_file"], "evidence/E1.sh")
        self.assertIn("list-invoice-summaries", (run_dir / "evidence/E1.sh").read_text())

    def test_unknown_kind_refused(self):
        with self.assertRaises(RecordError):
            add_evidence(self.new_run(), kind="guess", source="x", command="x", raw=b"", ext="txt")

    def test_provided_figure(self):
        run_dir = self.new_run()
        self.assertEqual(add_provided(run_dir, description="October invoice before tax", value="56940.00", currency="USD", given_by="owner"), "P1")
        entry = load_run(run_dir)["provided"][0]
        self.assertEqual(entry["value"], "56940.00")
        self.assertEqual(entry["given_by"], "owner")
        self.assertTrue(entry["given_at"])

    def test_provided_value_must_be_a_number(self):
        with self.assertRaises(RecordError):
            add_provided(self.new_run(), description="x", value="abc", currency="USD", given_by="owner")


class Locate(RecordCase):
    def write(self, name, text):
        path = self.tmp / name
        path.write_text(text)
        return path

    CSV = {"type": "csv", "where": {"billing_period": "2026-10"}, "column": "amount"}

    def test_csv_row(self):
        path = self.write("a.csv", "billing_period,amount\n2026-09,1.00\n2026-10,56938.72\n")
        self.assertEqual(locate(path, self.CSV), "56938.72")

    def test_scientific_notation_returned_unchanged(self):
        path = self.write("a.csv", "billing_period,amount\n2026-10,1.2345E7\n")
        self.assertEqual(locate(path, self.CSV), "1.2345E7")

    def test_no_match_more_than_one_header_only_and_empty_cell(self):
        for text in (
            "billing_period,amount\n2026-09,1.00\n",
            "billing_period,amount\n2026-10,1.00\n2026-10,2.00\n",
            "billing_period,amount\n",
            "billing_period,amount\n2026-10,\n",
        ):
            with self.subTest(text=text), self.assertRaises(LocatorError):
                locate(self.write("a.csv", text), self.CSV)

    def test_missing_column(self):
        path = self.write("a.csv", "billing_period,cost\n2026-10,1.00\n")
        with self.assertRaises(LocatorError):
            locate(path, self.CSV)

    def test_json_path_keeps_the_source_text_of_numbers(self):
        path = self.write("a.json", '{"items": [{"amount": "56940.00", "saving": 1900.10, "big": 1.2345E7}]}')
        self.assertEqual(locate(path, {"type": "json", "path": ["items", 0, "amount"]}), "56940.00")
        self.assertEqual(locate(path, {"type": "json", "path": ["items", 0, "saving"]}), "1900.10")
        self.assertEqual(locate(path, {"type": "json", "path": ["items", 0, "big"]}), "1.2345E7")

    def test_json_bad_path_null_and_container_raise(self):
        path = self.write("a.json", '{"items": [{"amount": null}]}')
        for walk in (["items", 1], ["nope"], ["items", 0, "amount"], ["items", 0], ["items", "x"]):
            with self.subTest(path=walk), self.assertRaises(LocatorError):
                locate(path, {"type": "json", "path": walk})

    def test_unknown_locator_type(self):
        with self.assertRaises(LocatorError):
            locate(self.write("a.csv", "x\n1\n"), {"type": "xml"})


class Merge(RecordCase):
    def test_refuses_script_owned_keys(self):
        run_dir = self.new_run()
        for key in ("scope", "budget", "evidence", "provided"):
            with self.subTest(key=key), self.assertRaises(RecordError):
                merge(run_dir, {key: {}})

    def test_refuses_unknown_keys(self):
        with self.assertRaises(RecordError):
            merge(self.new_run(), {"status": "Reconciled to invoice"})

    def test_merges_by_section(self):
        run_dir = self.new_run()
        merge(run_dir, {"figures": {"a": {"value": "1"}}, "calculations": [{"id": "C1", "name": "sum"}], "gaps": ["one"], "savings_total": "C1.total"})
        merge(run_dir, {"figures": {"b": {"value": "2"}}, "calculations": [{"id": "C1", "name": "change"}, {"id": "C2", "name": "sum"}], "gaps": ["one", "two"]})
        run = load_run(run_dir)
        self.assertEqual(sorted(run["figures"]), ["a", "b"])
        self.assertEqual([(c["id"], c["name"]) for c in run["calculations"]], [("C1", "change"), ("C2", "sum")])
        self.assertEqual(run["gaps"], ["one", "two"])
        self.assertEqual(run["savings_total"], "C1.total")

    def test_reconciliation_replaces_by_period(self):
        run_dir = self.new_run()
        merge(run_dir, {"reconciliation": [{"period": "2026-10", "invoice": None}]})
        merge(run_dir, {"reconciliation": [{"period": "2026-10", "invoice": "x"}, {"period": "2026-09", "invoice": None}]})
        self.assertEqual([r["invoice"] for r in load_run(run_dir)["reconciliation"]], ["x", None])


class Scope(RecordCase):
    def test_update_scope_allows_only_named_fields(self):
        run_dir = self.new_run()
        update_scope(run_dir, data_through="2026-11-03", last_report="docs/finops/x/report.md")
        scope = load_run(run_dir)["scope"]
        self.assertEqual(scope["data_through"], "2026-11-03")
        self.assertEqual(scope["last_report"], "docs/finops/x/report.md")
        with self.assertRaises(RecordError):
            update_scope(run_dir, currency="EUR")
        with self.assertRaises(RecordError):
            update_scope(run_dir, data_through="soon")

    def test_add_gap_keeps_one_copy(self):
        run_dir = self.new_run()
        add_gap(run_dir, "no CUR")
        add_gap(run_dir, "no CUR")
        self.assertEqual(load_run(run_dir)["gaps"], ["no CUR"])


class Durability(RecordCase):
    def test_failed_save_leaves_the_previous_record(self):
        run_dir = self.new_run()
        add_gap(run_dir, "kept")
        run = load_run(run_dir)
        run["gaps"].append("lost")
        with mock.patch.object(finops_record.os, "replace", side_effect=OSError("disk full")):
            with self.assertRaises(OSError):
                save_run(run_dir, run)
        self.assertEqual(load_run(run_dir)["gaps"], ["kept"])
        self.assertEqual(sorted(p.name for p in run_dir.iterdir()), ["evidence", "run.json"])

    def test_corrupt_record_is_a_record_error(self):
        run_dir = self.new_run()
        (run_dir / "run.json").write_text("{not json")
        with self.assertRaises(RecordError):
            load_run(run_dir)


class Helpers(unittest.TestCase):
    def test_is_closed(self):
        self.assertTrue(is_closed("2026-10", date(2026, 11, 1)))
        self.assertFalse(is_closed("2026-10", date(2026, 10, 31)))
        self.assertTrue(is_closed("2026-10-01_2026-10-07", date(2026, 10, 8)))
        self.assertFalse(is_closed("2026-10-01_2026-10-07", date(2026, 10, 7)))

    def test_sanitize_strips_control_characters_only(self):
        self.assertEqual(sanitize("a\x1b[2J\r\tb\nc\x00\x7f\x9b"), "a[2J\tb\nc")


class Cli(RecordCase):
    INIT = ("init", "--provider", "aws", "--billing-scope", "123456789012", "--scope-kind", "whole-bill", "--period", "2026-10", "--comparison", "2026-09", "--currency", "USD", "--question", "q")

    def git_repo(self):
        repo = self.tmp / "repo"
        repo.mkdir()
        subprocess.run(["git", "init", "-q", str(repo)], check=True)
        return repo

    def test_warns_when_base_is_not_ignored_and_leaves_gitignore_alone(self):
        repo = self.git_repo()
        done = run_script("finops_record.py", *self.INIT, "--base", "docs/finops", cwd=repo)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertTrue(any(line.startswith("WARN: ") for line in done.stdout.splitlines()), done.stdout)
        self.assertFalse((repo / ".gitignore").exists())

    def test_no_warning_when_ignored(self):
        repo = self.git_repo()
        (repo / ".gitignore").write_text("docs/finops/\n")
        done = run_script("finops_record.py", *self.INIT, "--base", "docs/finops", cwd=repo)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertNotIn("WARN: ", done.stdout)
        self.assertTrue(Path(done.stdout.strip().splitlines()[-1]).is_absolute() or (repo / done.stdout.strip().splitlines()[-1]).is_dir())

    def test_unsafe_scope_exits_2(self):
        args = list(self.INIT)
        args[args.index("123456789012")] = "../x"
        done = run_script("finops_record.py", *args, "--base", str(self.base))
        self.assertEqual(done.returncode, 2)

    def test_budget_provided_reported_gap_scope_and_merge(self):
        run_dir = self.new_run()
        self.assertEqual(run_script("finops_record.py", "approve-budget", run_dir, "--athena-bytes", "5", "--bigquery-bytes", "6", "--ce-requests", "7").returncode, 0)
        budget = load_run(run_dir)["budget"]
        self.assertTrue(budget["approved"])
        self.assertEqual(budget["limits"], {"athena_bytes": 5, "bigquery_bytes": 6, "ce_requests": 7})

        done = run_script("finops_record.py", "provided", run_dir, "--description", "invoice", "--value", "10.00", "--currency", "USD", "--given-by", "owner")
        self.assertIn("P1", done.stdout)

        findings = self.tmp / "findings.txt"
        findings.write_text("error rate flat")
        done = run_script("finops_record.py", "reported", run_dir, "--agent", "sre-metrics-analyst", "--file", findings)
        self.assertEqual(done.returncode, 0, done.stderr)
        entry = load_run(run_dir)["evidence"][0]
        self.assertEqual(entry["kind"], "reported")
        self.assertIn("sre-metrics-analyst", entry["source"])

        self.assertEqual(run_script("finops_record.py", "add-gap", run_dir, "no CUR").returncode, 0)
        self.assertEqual(run_script("finops_record.py", "set-scope", run_dir, "--data-through", "2026-11-03").returncode, 0)
        fragment = self.tmp / "fragment.json"
        fragment.write_text(json.dumps({"gaps": ["second"]}))
        self.assertEqual(run_script("finops_record.py", "merge", run_dir, fragment).returncode, 0)
        run = load_run(run_dir)
        self.assertEqual(run["gaps"], ["no CUR", "second"])
        self.assertEqual(run["scope"]["data_through"], "2026-11-03")

        fragment.write_text(json.dumps({"evidence": []}))
        self.assertEqual(run_script("finops_record.py", "merge", run_dir, fragment).returncode, 2)


if __name__ == "__main__":
    unittest.main()
