"""Problems found by a code review of the finished branch, one class for each."""
from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from concurrent.futures import ProcessPoolExecutor
from datetime import date
from pathlib import Path

from support import SCRIPTS, add_provided_figure, build_valid_run, recompute, run_script
from test_compare import make_prior
from test_proof_bypasses import Runners

import finops_guard
from finops_guard import GuardError, add_pending, check_bounded, record_use
from finops_record import RecordError, add_evidence, init_run, load_run, merge, save_run
from finops_report import render
from finops_validate import validate

SQL = SCRIPTS.parent / "sql"
PARTITION = "billing_period"


class ValidRunCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp = Path(self._tmp.name)
        self.run_dir = build_valid_run(self.tmp)

    def edit(self, change):
        run = load_run(self.run_dir)
        change(run)
        save_run(self.run_dir, run)

    def codes(self):
        return {failure.code for failure in validate(self.run_dir)}


class GcpInvoiceMonth(Runners):
    """The export stores invoice.month as YYYYMM, and a run's periods are YYYY-MM."""

    def query_with(self, command, *params):
        args = [command, self.run_dir, "--project", "p", "--location", "US", "--query-file"]
        return self.gcp(*args, *params)

    def test_a_compact_month_is_refused_before_any_bq_call(self):
        self.approve()
        for command in ("estimate", "query"):
            with self.subTest(command=command):
                done = self.query_with(command, SQL / "gcp-invoice-total.sql", "--param", "table=p.d.t", "--param", "invoice_month=202610")
                self.assertEqual(done.returncode, 2, done.stdout)
                self.assertIn("YYYY-MM", done.stderr)
        self.assertEqual(self.scenario.called("bq"), [])

    def test_both_commitment_months_are_checked(self):
        self.approve()
        done = self.query_with("query", SQL / "gcp-cud.sql", "--param", "table=p.d.t", "--param", "first_month=2026-08", "--param", "second_month=202609")
        self.assertEqual(done.returncode, 2, done.stdout)
        self.assertEqual(self.scenario.called("bq"), [])


class PartitionFilter(Runners):
    """A filter only bounds a scan from the WHERE of the block that reads the table."""

    def refuses(self, sql):
        with self.assertRaises(GuardError, msg=sql):
            check_bounded(sql, PARTITION)

    def test_a_partition_column_outside_where_does_not_count(self):
        self.refuses("SELECT billing_period, SUM(c) AS c FROM t WHERE line_item_type <> 'Tax' GROUP BY billing_period")
        self.refuses("SELECT a FROM t WHERE b = 1 ORDER BY billing_period")
        self.refuses("SELECT a, COUNT(a) AS n FROM t WHERE b = 1 GROUP BY a HAVING MAX(billing_period) > 'x'")

    def test_a_filter_in_another_block_does_not_count(self):
        self.refuses("WITH a AS (SELECT billing_period, c FROM t WHERE c > 0) SELECT billing_period FROM a")
        self.refuses("SELECT a FROM t WHERE b IN (SELECT b FROM u WHERE billing_period = 'x')")
        self.refuses("SELECT (SELECT MAX(c) FROM u) AS m FROM t WHERE billing_period = 'x'")

    def test_every_block_that_reads_a_table_needs_its_own_filter(self):
        self.refuses("SELECT a FROM t WHERE billing_period = 'x' UNION ALL SELECT a FROM u")
        self.refuses("SELECT a FROM (SELECT a FROM t) AS s WHERE billing_period = 'x'")

    def test_a_parenthesized_join_counts_as_a_table(self):
        self.refuses("SELECT a FROM (t JOIN u ON t.k = u.k)")

    def test_blocks_that_only_read_a_filtered_block_pass(self):
        check_bounded("WITH a AS (SELECT c FROM t WHERE billing_period = 'x'), b AS (SELECT c FROM a) SELECT c FROM b", PARTITION)
        check_bounded("SELECT c FROM (SELECT c FROM t WHERE billing_period = 'x') AS s", PARTITION)

    def test_ordinary_filters_still_pass(self):
        check_bounded("SELECT a FROM t WHERE (billing_period = 'x' AND b = 1)", PARTITION)
        check_bounded("SELECT a FROM db.t AS s WHERE s.billing_period = 'x'", PARTITION)
        check_bounded("SELECT a FROM t WHERE billing_period = 'x' GROUP BY a ORDER BY a", PARTITION)
        check_bounded("select a from t where BILLING_PERIOD = 'x'", PARTITION)

    def test_unnest_is_not_a_table(self):
        check_bounded("SELECT a, (SELECT SUM(v) FROM UNNEST(x) AS u) AS s FROM t WHERE billing_period = 'x'", PARTITION)
        check_bounded("SELECT a FROM t CROSS JOIN UNNEST(x) AS u(v) WHERE billing_period = 'x'", PARTITION)

    def test_a_function_using_from_is_not_a_read(self):
        check_bounded("SELECT EXTRACT(year FROM d) AS y FROM t WHERE billing_period = 'x'", PARTITION)

    def test_the_athena_runner_refuses_before_starting_anything(self):
        self.approve()
        query = self.tmp / "unbounded.sql"
        query.write_text("SELECT billing_period, SUM(c) AS c FROM {{table}} WHERE c > 0 GROUP BY billing_period")
        done = self.aws("athena", self.run_dir, "--workgroup", "finops", "--database", "billing", "--query-file", query, "--param", "table=cur")
        self.assertEqual(done.returncode, 2, done.stdout)
        self.assertEqual(self.scenario.called("aws", "start-query-execution"), [])


def _add_evidence(item):
    run_dir, index = item
    add_evidence(Path(run_dir), kind="billing", source=f"source-{index}", command="c", raw=f"raw-{index}\n".encode(), ext="csv")


def _use_budget(run_dir):
    record_use(Path(run_dir), "ce_requests", 1)


class OverlappingCalls(unittest.TestCase):
    """Nothing stops an agent running two script calls at once."""

    COUNT = 48

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp = Path(self._tmp.name)
        self.run_dir = init_run(self.tmp, provider="aws", billing_scope="123456789012", scope_kind="partial", period="2026-10", comparison_period=None, currency="USD", question="q", today=date(2026, 11, 5))

    def test_overlapping_evidence_keeps_every_entry_and_its_raw_file(self):
        with ProcessPoolExecutor(8) as pool:
            list(pool.map(_add_evidence, [(str(self.run_dir), index) for index in range(self.COUNT)]))
        evidence = load_run(self.run_dir)["evidence"]
        self.assertEqual(sorted(entry["id"] for entry in evidence), sorted(f"E{index + 1}" for index in range(self.COUNT)))
        self.assertEqual({entry["source"] for entry in evidence}, {f"source-{index}" for index in range(self.COUNT)})
        self.assertEqual(validate(self.run_dir), [])

    def test_overlapping_budget_use_all_counts(self):
        with ProcessPoolExecutor(8) as pool:
            list(pool.map(_use_budget, [str(self.run_dir)] * self.COUNT))
        self.assertEqual(load_run(self.run_dir)["budget"]["used"]["ce_requests"], self.COUNT)

    def test_a_finished_query_seen_by_two_calls_is_charged_once(self):
        add_pending(self.run_dir, "athena", "qid-1")
        finops_guard.settle_query(self.run_dir, "athena_bytes", "qid-1", 700)
        finops_guard.settle_query(self.run_dir, "athena_bytes", "qid-1", 700)
        budget = load_run(self.run_dir)["budget"]
        self.assertEqual(budget["used"]["athena_bytes"], 700)
        self.assertEqual(budget["pending"], [])

    def test_a_failed_change_saves_nothing(self):
        with self.assertRaises(RecordError):
            merge(self.run_dir, {"gaps": ["kept out"], "not_a_section": []})
        self.assertEqual(load_run(self.run_dir)["gaps"], [])

    def test_a_missing_run_directory_is_a_record_error(self):
        with self.assertRaises(RecordError):
            record_use(self.tmp / "nope", "ce_requests", 1)


class BigQueryJobAfterAFailure(Runners):
    """--job_id is set by the client, so a job can exist and bill even when bq exits non-zero."""

    REAL_RUN = "--max_rows=1000000"

    def dry_run(self):
        self.scenario.on("bq", "query", "--dry_run", stdout=json.dumps({"statistics": {"query": {"totalBytesProcessed": "1000"}}}))

    def fail_then_succeed(self):
        self.dry_run()
        self.scenario.on("bq", "query", self.REAL_RUN, stderr="Connection reset by peer", exit=1)
        self.scenario.on("bq", "query", self.REAL_RUN, stdout="a\n1\n")

    def test_a_client_side_failure_keeps_the_job_pending_and_says_so(self):
        self.approve()
        self.fail_then_succeed()
        done = self.bq_query()
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertRegex(done.stdout, r"(?m)^GAP: bq query .*Connection reset.*may have started")
        pending = load_run(self.run_dir)["budget"]["pending"]
        self.assertEqual(len(pending), 1)
        self.assertIn(pending[0]["job_id"], done.stdout)

    def test_the_next_query_charges_what_the_job_really_billed(self):
        self.approve()
        self.fail_then_succeed()
        self.bq_query()
        self.scenario.on("bq", "show", "-j", stdout=json.dumps({"status": {"state": "DONE"}, "statistics": {"query": {"totalBytesBilled": "4000"}}}))
        self.scenario.on("bq", "show", "-j", stdout=json.dumps({"status": {"state": "DONE"}, "statistics": {"query": {"totalBytesBilled": "5"}}}))
        done = self.bq_query()
        self.assertEqual(done.returncode, 0, done.stdout)
        self.assertEqual(self.used("bigquery_bytes"), 4005)
        self.assertEqual(load_run(self.run_dir)["budget"]["pending"], [])

    def test_a_job_that_failed_on_the_server_charges_nothing(self):
        self.approve()
        self.fail_then_succeed()
        self.bq_query()
        self.scenario.on("bq", "show", "-j", stdout=json.dumps({"status": {"state": "DONE", "errorResult": {"reason": "accessDenied"}}, "statistics": {}}))
        self.scenario.on("bq", "show", "-j", stdout=json.dumps({"status": {"state": "DONE"}, "statistics": {"query": {"totalBytesBilled": "5"}}}))
        done = self.bq_query()
        self.assertEqual(done.returncode, 0, done.stdout)
        self.assertEqual(self.used("bigquery_bytes"), 5)
        self.assertFalse([item for item in load_run(self.run_dir)["gaps"] if "dry-run estimate" in str(item)])

    def test_the_bytes_limit_failure_is_known_to_be_free(self):
        self.approve()
        self.dry_run()
        self.scenario.on("bq", "query", self.REAL_RUN, stderr="Query exceeded limit for bytes billed: 5", exit=2)
        self.assertEqual(self.bq_query().returncode, 3)
        self.assertEqual(load_run(self.run_dir)["budget"]["pending"], [])

    def test_a_job_is_pending_until_the_moment_it_is_charged(self):
        self.approve()
        self.dry_run()
        self.scenario.on("bq", "query", self.REAL_RUN, stdout="a\n1\n")
        self.scenario.on("bq", "show", "-j", stdout=json.dumps({"statistics": {"query": {"totalBytesBilled": "9"}}}))
        self.assertEqual(self.bq_query().returncode, 0)
        budget = load_run(self.run_dir)["budget"]
        self.assertEqual(budget["used"]["bigquery_bytes"], 9)
        self.assertEqual(budget["pending"], [])


class BasisIsProved(ValidRunCase):
    """The figure's label is the model's. The evidence's basis is the script's."""

    def evidence_basis(self, value):
        def change(run):
            next(entry for entry in run["evidence"] if entry["id"] == "E3")["basis"] = value

        return change

    def test_effective_evidence_cant_be_a_query_total(self):
        self.edit(self.evidence_basis("effective"))
        codes = self.codes()
        self.assertIn("RECON_ROLE", codes)
        self.assertIn("FIGURE_KIND", codes)

    def test_evidence_with_no_basis_cant_be_a_query_total(self):
        self.edit(self.evidence_basis(""))
        self.assertIn("RECON_ROLE", self.codes())

    def test_a_label_has_to_match_its_evidence(self):
        def change(run):
            next(entry for entry in run["evidence"] if entry["id"] == "E7")["basis"] = "avoidable"
            run["figures"]["e7_search"]["basis"] = "billed"

        self.edit(change)
        self.assertIn("FIGURE_KIND", self.codes())

    def test_the_report_cant_be_labeled_billed_on_effective_evidence(self):
        self.edit(lambda run: run["figures"]["oct_no_owner"].update(basis="effective"))
        self.assertIn("FIGURE_KIND", self.codes())


class RunnersRecordTheBasis(Runners):
    CE = ("--start", "2026-10-01", "--end", "2026-11-01", "--granularity", "MONTHLY")

    def stub_athena(self):
        s = self.scenario
        s.on("aws", "athena", "start-query-execution", stdout=json.dumps({"QueryExecutionId": "qid-1"}))
        s.on("aws", "athena", "get-query-execution", stdout=json.dumps({"QueryExecution": {"Status": {"State": "SUCCEEDED"}, "Statistics": {"DataScannedInBytes": 5}}}))
        s.on("aws", "athena", "get-query-results", stdout=json.dumps({"ResultSet": {"Rows": [{"Data": [{"VarCharValue": "a"}]}, {"Data": [{"VarCharValue": "1"}]}]}}))

    def test_cost_explorer_basis_follows_the_metric(self):
        self.approve()
        self.scenario.on("aws", "ce", "get-cost-and-usage", stdout='{"ResultsByTime": []}')
        for metric, basis in (("UnblendedCost", "billed"), ("NetUnblendedCost", "billed"), ("AmortizedCost", "effective"), ("NetAmortizedCost", "effective"), ("BlendedCost", ""), ("UsageQuantity", "")):
            with self.subTest(metric=metric):
                done = self.aws("ce", self.run_dir, *self.CE, "--metric", metric)
                self.assertEqual(done.returncode, 0, done.stderr)
                self.assertEqual(self.evidence()[-1]["basis"], basis)

    def test_cost_explorer_has_no_flag_to_contradict_the_metric(self):
        self.approve()
        self.assertEqual(self.aws("ce", self.run_dir, *self.CE, "--metric", "AmortizedCost", "--basis", "billed").returncode, 2)

    def test_an_effective_athena_query_is_recorded_as_effective(self):
        self.approve()
        self.stub_athena()
        done = self.aws("athena", self.run_dir, "--workgroup", "finops", "--database", "billing", "--query-file", SQL / "aws-cur2-effective.sql", "--param", "table=cur", "--param", "period=2026-10")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(self.evidence()[-1]["basis"], "effective")

    def test_a_billed_athena_query_is_recorded_as_billed(self):
        self.approve()
        self.stub_athena()
        self.athena()
        self.assertEqual(self.evidence()[-1]["basis"], "billed")

    def test_an_effective_bigquery_query_is_recorded_as_effective(self):
        self.approve()
        s = self.scenario
        s.on("bq", "query", "--dry_run", stdout=json.dumps({"statistics": {"query": {"totalBytesProcessed": "1"}}}))
        s.on("bq", "query", "--max_rows=1000000", stdout="a\n1\n")
        s.on("bq", "show", "-j", stdout=json.dumps({"statistics": {"query": {"totalBytesBilled": "1"}}}))
        done = self.gcp("query", self.run_dir, "--project", "p", "--location", "US", "--query-file", SQL / "gcp-effective.sql", "--param", "table=p.d.t", "--param", "usage_start=2026-10-01", "--param", "usage_end=2026-11-01")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(self.evidence()[-1]["basis"], "effective")


class SavingsWindow(ValidRunCase):
    """O-12 changed on 2026-07-15, so its baseline is 2026-06 and its post month is 2026-08."""

    def use(self, **inputs):
        def change(run):
            calc = next(item for item in run["calculations"] if item["id"] == "C11")
            calc["inputs"].update(inputs)
            recompute(run, calc)

        self.edit(change)

    def test_the_right_months_pass(self):
        self.assertEqual(validate(self.run_dir), [])

    def test_a_baseline_from_another_month_fails(self):
        self.use(baseline_cost="sep_query_total")
        self.assertIn("PAST_WINDOW", self.codes())

    def test_a_post_month_other_than_the_first_after_the_change_fails(self):
        self.use(post_cost="sep_query_total")
        self.assertIn("PAST_WINDOW", self.codes())

    def test_swapped_months_fail(self):
        self.use(baseline_cost="gp3_post", post_cost="gp3_baseline")
        self.assertIn("PAST_WINDOW", self.codes())

    def test_a_change_date_that_doesnt_fit_the_months_fails(self):
        self.edit(lambda run: run["past_savings"][0].update(implemented_on="2026-06-15"))
        self.assertIn("PAST_WINDOW", self.codes())

    def test_usage_inputs_are_checked_too(self):
        self.use(baseline_usage="sep_nat_gb", post_usage="oct_nat_gb")
        self.assertIn("PAST_WINDOW", self.codes())

    def test_a_calculation_is_followed_to_its_figures(self):
        def change(run):
            total = {"id": "C30", "name": "sum", "kind": "calculated", "assumptions": [], "inputs": {"terms": ["sep_no_owner"]}}
            recompute(run, total)
            run["calculations"].append(total)

        self.edit(change)
        self.use(baseline_cost="C30.total")
        self.assertIn("PAST_WINDOW", self.codes())

    def test_a_figure_with_no_known_period_fails(self):
        evidence = add_evidence(self.run_dir, kind="billing", source="ad hoc", command="c", raw=b"a\n5\n", ext="csv")
        merge(self.run_dir, {"figures": {"unknown_month": {"value": "5", "kind": "measured", "currency": "USD", "basis": "billed", "evidence": evidence, "locator": {"type": "csv", "where": {"a": "5"}, "column": "a"}}}})
        self.use(baseline_cost="unknown_month")
        self.assertIn("PAST_WINDOW", self.codes())

    def test_figures_a_person_typed_have_no_period_to_check(self):
        def change(run):
            add_provided_figure(run, "typed_baseline", "3000.00")
            add_provided_figure(run, "typed_post", "2220.00")

        self.edit(change)
        self.use(baseline_cost="typed_baseline", post_cost="typed_post")
        self.assertEqual(validate(self.run_dir), [])


class RestateTwice(ValidRunCase):
    """SKILL.md says to fix the record and run the step again, so repeats are normal."""

    def setUp(self):
        super().setUp()
        self.base = self.tmp / "docs/finops"
        make_prior(self.base, query_total="48250.00")
        run_script("finops_compare.py", "carry", self.run_dir, "--base", self.base)

    def restate(self):
        done = run_script("finops_compare.py", "restate", self.run_dir)
        self.assertEqual(done.returncode, 0, done.stderr)
        return done

    def restated(self):
        run = load_run(self.run_dir)
        calcs = [calc for calc in run["calculations"] if calc.get("inputs", {}).get("before") == "prior_query_total_2026-09"]
        gaps = [item for item in run["gaps"] if isinstance(item, dict)]
        return calcs, gaps

    def test_running_it_again_adds_nothing(self):
        first = self.restate()
        self.restate()
        third = self.restate()
        calcs, gaps = self.restated()
        self.assertEqual(len(calcs), 1)
        self.assertEqual(len(gaps), 1)
        self.assertEqual(gaps[0]["ref"], f"{calcs[0]['id']}.change")
        self.assertEqual(first.stdout, third.stdout)
        self.assertEqual(validate(self.run_dir), [])

    def test_a_total_that_changes_between_runs_updates_the_same_calculation(self):
        self.restate()
        before, _ = self.restated()

        def change(run):
            evidence = next(entry for entry in run["evidence"] if entry["id"] == "E3")
            path = self.run_dir / evidence["raw_path"]
            path.write_bytes(path.read_bytes().replace(b"48209.40", b"48209.50"))
            evidence["sha256"] = hashlib.sha256(path.read_bytes()).hexdigest()
            run["figures"]["sep_query_total"]["value"] = "48209.50"

        self.edit(change)
        self.restate()
        after, gaps = self.restated()
        self.assertEqual(len(after), 1)
        self.assertEqual(len(gaps), 1)
        self.assertEqual(after[0]["id"], before[0]["id"])
        self.assertNotEqual(after[0]["outputs"], before[0]["outputs"])


class MoneyInTheReportsCurrency(ValidRunCase):
    def action_says(self, currency, text):
        def change(run):
            run["scope"]["currency"] = currency
            run["opportunities"][0]["action"] = text

        self.edit(change)

    def test_the_reports_currency_counts_as_money_either_way_round(self):
        for text in ("save 500 CAD a month", "save CAD 500 a month", "save CAD500 a month"):
            with self.subTest(text=text):
                self.action_says("CAD", text)
                self.assertIn("UNTAGGED_MONEY", self.codes())

    def test_a_word_starting_with_the_code_is_not_money(self):
        self.action_says("CAD", "Review CADENCE 3 weeks")
        self.assertNotIn("UNTAGGED_MONEY", self.codes())


class ReconciliationHeaders(ValidRunCase):
    def test_the_same_month_in_two_years_can_be_told_apart(self):
        run = load_run(self.run_dir)
        run["scope"]["comparison_period"] = "2025-10"
        run["reconciliation"][1]["period"] = "2025-10"
        self.assertIn("| Item | October 2026 | October 2025 | Source |", render(run))


if __name__ == "__main__":
    unittest.main()
