from __future__ import annotations

import tempfile
import unittest
from decimal import Decimal as D
from pathlib import Path

from support import add_ledger, add_provided_figure, build_valid_run, recompute, run_script

from finops_record import add_evidence, load_run, save_run
from finops_validate import derive_status, resolve, validate


class ValidateCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.run_dir = build_valid_run(Path(self._tmp.name))

    def edit(self, change):
        run = load_run(self.run_dir)
        change(run)
        save_run(self.run_dir, run)

    def codes(self):
        return {failure.code for failure in validate(self.run_dir)}

    def assert_fails(self, code, change=None):
        if change:
            self.edit(change)
        self.assertIn(code, self.codes())

    def figure(self, run, name):
        return run["figures"][name]

    def calc(self, run, calc_id):
        return next(c for c in run["calculations"] if c["id"] == calc_id)


class ValidRun(ValidateCase):
    def test_design_report_passes(self):
        self.assertEqual(validate(self.run_dir), [])

    def test_the_fixture_holds_the_designs_numbers(self):
        run = load_run(self.run_dir)
        self.assertEqual(D(resolve(run, "C1.residual").value), D("1.28"))
        self.assertEqual(D(resolve(run, "C2.residual").value), D("0.60"))
        self.assertEqual(D(resolve(run, "C5.change").value), D("8729.32"))
        self.assertEqual(D(resolve(run, "C6.change").value), D("4500"))
        self.assertEqual(D(resolve(run, "C7.change").value), D("3100"))
        self.assertEqual(D(resolve(run, "C8.change").value), D("1130"))
        self.assertEqual(D(resolve(run, "C10.residual").value), D("-0.68"))
        self.assertEqual(D(resolve(run, "C11.saving").value), D("780"))
        self.assertEqual(D(resolve(run, "C14.total").value), D("6140"))

    def test_resolve_builds_tags(self):
        run = load_run(self.run_dir)
        self.assertEqual(resolve(run, "oct_query_total").tag, "E3")
        self.assertEqual(resolve(run, "e7_search").tag, "E7 est")
        self.assertEqual(resolve(run, "C1.residual").tag, "C1")
        self.assertEqual(resolve(run, "C12.total").tag, "C12 est")
        self.assertEqual(resolve(run, "oct_query_total").currency, "USD")


class Evidence(ValidateCase):
    def test_edited_raw_result(self):
        with (self.run_dir / "evidence/E3.csv").open("ab") as raw:
            raw.write(b"x")
        self.assertIn("HASH_MISMATCH", self.codes())

    def test_deleted_raw_result(self):
        (self.run_dir / "evidence/E1.json").unlink()
        self.assertIn("RAW_MISSING", self.codes())

    def test_typed_figure_differs_from_raw_result(self):
        self.assert_fails("FIGURE_MISMATCH", lambda run: self.figure(run, "oct_query_total").update(value="56938.73"))

    def test_equal_number_in_another_form_is_the_same_figure(self):
        self.edit(lambda run: self.figure(run, "oct_query_total").update(value="56938.720"))
        self.assertNotIn("FIGURE_MISMATCH", self.codes())

    def test_locator_finds_nothing(self):
        self.assert_fails("LOCATOR_FAILED", lambda run: self.figure(run, "oct_query_total")["locator"]["where"].update(billing_period="1999-01"))

    def test_incomplete_evidence_can_not_back_a_figure(self):
        self.assert_fails("EVIDENCE_INCOMPLETE", lambda run: run["evidence"][2].update(complete=False))

    def test_reported_evidence_can_not_back_a_figure(self):
        self.assert_fails("FIGURE_KIND", lambda run: run["evidence"][6].update(kind="reported"))

    def test_measured_figure_on_a_cost_tool_allocation(self):
        def change(run):
            run["evidence"][2]["kind"] = "allocation"

        self.assert_fails("FIGURE_KIND", change)

    def test_measured_figure_on_a_recommendation(self):
        self.assert_fails("FIGURE_KIND", lambda run: self.figure(run, "e7_search").update(kind="measured"))

    def test_figure_cites_unknown_evidence(self):
        self.assert_fails("UNRESOLVED_REF", lambda run: self.figure(run, "oct_query_total").update(evidence="E99"))

    def test_provided_figure_must_match_its_entry(self):
        def change(run):
            run["provided"].append({"id": "P1", "description": "invoice", "value": "56940.00", "currency": "USD", "given_by": "owner", "given_at": "2026-11-05T00:00:00Z"})
            run["figures"]["oct_invoice"] = {"value": "56941.00", "kind": "provided", "currency": "USD", "basis": "billed", "provided": "P1"}

        self.assert_fails("FIGURE_MISMATCH", change)

    def test_provided_figure_with_no_entry(self):
        def change(run):
            run["figures"]["oct_invoice"] = {"value": "56940.00", "kind": "provided", "currency": "USD", "basis": "billed", "provided": "P9"}

        self.assert_fails("UNRESOLVED_REF", change)


class Refs(ValidateCase):
    def test_section_ref_with_no_entry(self):
        self.assert_fails("UNRESOLVED_REF", lambda run: run["changes"].update(total="C99.change"))

    def test_ref_to_a_missing_output(self):
        self.assert_fails("UNRESOLVED_REF", lambda run: run["changes"].update(total="C5.nope"))

    def test_cause_cites_unknown_evidence(self):
        self.assert_fails("UNRESOLVED_REF", lambda run: run["changes"]["drivers"][0]["cause"].update(evidence=["E99"]))

    def test_gap_ref_with_no_entry(self):
        self.assert_fails("UNRESOLVED_REF", lambda run: run["gaps"].append({"text": "restated", "ref": "C99.change"}))


class FreeText(ValidateCase):
    def test_money_in_a_gap(self):
        self.assert_fails("UNTAGGED_MONEY", lambda run: run["gaps"].append("cost $3,600 more"))

    def test_money_in_an_action(self):
        self.assert_fails("UNTAGGED_MONEY", lambda run: run["opportunities"][0].update(action="saves 3600 USD"))

    def test_money_in_an_assumption(self):
        self.assert_fails("UNTAGGED_MONEY", lambda run: run["opportunities"][0].update(assumes=["EUR 12 per GB"]))

    def test_money_in_a_cause(self):
        self.assert_fails("UNTAGGED_MONEY", lambda run: run["changes"]["drivers"][0]["cause"].update(text="cost £40 more"))

    def test_a_version_number_is_not_money(self):
        self.assertEqual(validate(self.run_dir), [])
        self.assertIn("v2.14", load_run(self.run_dir)["changes"]["drivers"][0]["cause"]["text"])


class Calculations(ValidateCase):
    def test_stored_output_differs(self):
        self.assert_fails("CALC_MISMATCH", lambda run: self.calc(run, "C5")["outputs"].update(change="1.00"))

    def test_unknown_calculation(self):
        self.assert_fails("CALC_UNKNOWN", lambda run: self.calc(run, "C5").update(name="forecast"))

    def test_literal_input_in_a_calculated_figure(self):
        self.assert_fails("CALC_KIND", lambda run: self.calc(run, "C12").update(kind="calculated"))

    def test_estimated_input_in_a_calculated_figure(self):
        self.assert_fails("CALC_KIND", lambda run: self.calc(run, "C14").update(kind="calculated"))

    def test_literal_with_no_assumption(self):
        self.assert_fails("CALC_KIND", lambda run: self.calc(run, "C12")["inputs"]["factors"][1].pop("assumption"))

    def test_mixed_currencies_in_one_sum(self):
        self.assert_fails("CURRENCY_MIX", lambda run: self.figure(run, "e7_search").update(currency="EUR"))

    def test_uncomputable_inputs_are_reported(self):
        self.assert_fails("CALC_MISMATCH", lambda run: self.calc(run, "C5")["inputs"].pop("before"))


class Conservation(ValidateCase):
    def test_drivers_must_sum_to_the_total_change(self):
        self.assert_fails("DRIVER_SUM", lambda run: run["changes"]["drivers"].pop())

    def test_residual_row_must_match_its_period(self):
        self.assert_fails("RECON_RESIDUAL", lambda run: run["reconciliation"][0].update(residual="C2.residual"))


class Ledger(ValidateCase):
    """The design's $1,000 a day example: $700 direct, $100 shared, $200 idle, split 2:1."""

    def test_consistent_ledger_passes(self):
        self.edit(add_ledger)
        self.assertEqual(validate(self.run_dir), [])
        run = load_run(self.run_dir)
        self.assertEqual(D(resolve(run, "C21.share_1").value), D(200))
        self.assertEqual(D(resolve(run, "C21.share_2").value), D(100))
        self.assertEqual(D(resolve(run, "C20.sum").value), D(1000))

    def test_covered_must_match_the_calculation(self):
        def change(run):
            add_ledger(run)
            add_provided_figure(run, "other_covered", "1300")
            run["kubernetes_ledger"]["covered"] = "other_covered"

        self.assert_fails("LEDGER_SUM", change)

    def test_rows_must_match_the_calculation(self):
        def change(run):
            add_ledger(run)
            run["kubernetes_ledger"]["rows"].pop(0)

        self.assert_fails("LEDGER_SUM", change)

    def test_a_node_charge_can_not_be_a_ledger_row(self):
        def change(run):
            add_ledger(run)
            run["kubernetes_ledger"]["rows"][0]["kind"] = "node"

        self.assert_fails("LEDGER_KIND", change)

    def test_a_distributed_pool_can_not_also_count(self):
        def change(run):
            add_ledger(run)
            for row in run["kubernetes_ledger"]["rows"]:
                if row["amount"] == "idle_pool":
                    row["distributed"] = False

        self.assert_fails("POOL_SUM", change)

    def test_shares_must_appear_as_rows(self):
        def change(run):
            add_ledger(run)
            ledger = run["kubernetes_ledger"]
            ledger["rows"] = [row for row in ledger["rows"] if row["amount"] != "C21.share_2"]

        self.assert_fails("POOL_SUM", change)


class Opportunities(ValidateCase):
    def test_same_resource_summed_without_an_overlap_calculation(self):
        self.assert_fails("OVERLAP", lambda run: run["opportunities"][2].update(resource="nat-gateway"))

    def test_overlap_calculation_allows_it(self):
        def change(run):
            run["opportunities"][2]["resource"] = "nat-gateway"
            overlap = {"id": "C30", "name": "overlap_savings", "kind": "estimated", "assumptions": [], "inputs": {"baseline": "oct_query_total", "reductions": [{"literal": "0.2", "assumption": "a"}, {"literal": "0.3", "assumption": "b"}]}}
            run["calculations"].append(overlap)
            recompute(run, overlap)
            run["opportunities"][0]["overlap_calc"] = "C30"
            run["opportunities"][2]["overlap_calc"] = "C30"
            total = self.calc(run, "C14")
            total["inputs"]["terms"] = ["C30.savings", "e7_search"]
            recompute(run, total)

        self.edit(change)
        self.assertNotIn("OVERLAP", self.codes())

    def test_overlap_calculation_does_not_excuse_summing_both(self):
        def change(run):
            run["opportunities"][2]["resource"] = "nat-gateway"
            overlap = {"id": "C30", "name": "overlap_savings", "kind": "estimated", "assumptions": [], "inputs": {"baseline": "oct_query_total", "reductions": [{"literal": "0.2", "assumption": "a"}]}}
            run["calculations"].append(overlap)
            recompute(run, overlap)
            run["opportunities"][0]["overlap_calc"] = "C30"
            run["opportunities"][2]["overlap_calc"] = "C30"

        self.assert_fails("OVERLAP", change)

    def test_overlap_calculation_must_exist(self):
        def change(run):
            run["opportunities"][2]["resource"] = "nat-gateway"
            run["opportunities"][0]["overlap_calc"] = "C99"
            run["opportunities"][2]["overlap_calc"] = "C99"

        self.assert_fails("OVERLAP", change)

    def test_rightsizing_needs_peak_evidence(self):
        self.assert_fails("PEAK_MISSING", lambda run: run["opportunities"][1].update(peak_evidence=None))

    def test_peak_evidence_must_be_telemetry(self):
        self.assert_fails("PEAK_MISSING", lambda run: run["opportunities"][1].update(peak_evidence="E3"))

    def drop_o22_from_total(self, run):
        calc = self.calc(run, "C14")
        calc["inputs"]["terms"] = ["C12.total", "C13.total"]
        recompute(run, calc)

    def test_investigation_item_outside_the_total_passes(self):
        def change(run):
            run["opportunities"][1].update(peak_evidence=None, needs_investigation=True)
            self.drop_o22_from_total(run)

        self.edit(change)
        self.assertEqual(validate(self.run_dir), [])

    def test_investigation_item_can_not_be_in_the_total(self):
        self.assert_fails("INVESTIGATE_IN_TOTAL", lambda run: run["opportunities"][1].update(peak_evidence=None, needs_investigation=True))


class Causes(ValidateCase):
    def test_cause_with_no_evidence(self):
        self.assert_fails("CAUSE_NO_EVIDENCE", lambda run: run["changes"]["drivers"][0]["cause"].update(evidence=[]))

    def test_no_cause_is_allowed(self):
        self.edit(lambda run: run["changes"]["drivers"][0].update(cause=None))
        self.assertEqual(validate(self.run_dir), [])

    def test_a_reported_finding_can_support_a_cause(self):
        add_evidence(self.run_dir, kind="reported", source="reported by sre-metrics-analyst", command="findings", raw=b"bytes doubled", ext="txt")
        self.edit(lambda run: run["changes"]["drivers"][0]["cause"].update(evidence=["E9"]))
        self.assertEqual(validate(self.run_dir), [])


class PastSavings(ValidateCase):
    def test_result_while_too_early(self):
        self.assert_fails("PAST_TOO_EARLY", lambda run: run["past_savings"][0].update(implemented_on="2026-10-15"))

    def test_result_must_be_a_realized_savings_output(self):
        self.assert_fails("PAST_RESULT_CALC", lambda run: run["past_savings"][0].update(result="C1.residual"))

    def test_too_early_with_no_result_passes(self):
        self.edit(lambda run: run["past_savings"][0].update(implemented_on="2026-10-15", result=None, status="too early to measure"))
        self.assertEqual(validate(self.run_dir), [])


class Fields(ValidateCase):
    def test_data_through_is_required(self):
        self.assert_fails("MISSING_FIELD", lambda run: run["scope"].update(data_through=None))

    def test_data_through_is_not_needed_when_nothing_was_measured(self):
        def change(run):
            run["scope"]["data_through"] = None
            run.update(figures={}, calculations=[], evidence=[], reconciliation=[], changes=None, past_savings=[], opportunities=[], savings_total=None)

        self.edit(change)
        self.assertEqual(validate(self.run_dir), [])

    def test_status_claim_that_does_not_hold(self):
        def change(run):
            run["scope"]["scope_kind"] = "partial"
            run["status"] = "Reconciled to invoice"

        self.assert_fails("STATUS_CLAIM", change)

    def test_claiming_a_future_month_is_closed(self):
        self.assert_fails("STATUS_CLAIM", lambda run: run["reconciliation"][0].update(period="2099-01"))

    def test_corrupt_record_is_a_failure_not_a_crash(self):
        (self.run_dir / "run.json").write_text("{not json")
        self.assertEqual({f.code for f in validate(self.run_dir)}, {"RECORD_INVALID"})


class Status(ValidateCase):
    def status(self, change=None):
        run = load_run(self.run_dir)
        if change:
            change(run)
        return derive_status(run)

    def test_whole_bill_closed_within_tolerance(self):
        self.assertEqual(self.status()[0], "Reconciled to invoice")

    def test_residual_outside_tolerance_names_the_period(self):
        status, reason = self.status(lambda run: run["figures"]["oct_invoice"].update(value="57100.00"))
        self.assertEqual(status, "Not reconciled")
        self.assertIn("2026-10", reason)

    def test_tolerance_boundary_is_inclusive(self):
        def at(value):
            def change(run):
                run["figures"]["oct_invoice"]["value"] = "100000"
                run["figures"]["oct_query_total"]["value"] = value

            return change

        self.assertEqual(self.status(at("99900"))[0], "Reconciled to invoice")
        self.assertEqual(self.status(at("99899.99"))[0], "Not reconciled")

    def test_query_above_invoice_is_also_outside(self):
        self.assertEqual(self.status(lambda run: run["figures"]["oct_query_total"].update(value="60000"))[0], "Not reconciled")

    def test_no_query_total_is_not_a_zero(self):
        status, reason = self.status(lambda run: run["reconciliation"][0].update(query_total=None))
        self.assertEqual(status, "Not reconciled")
        self.assertIn("2026-10", reason)

    def test_no_invoice_and_no_second_view(self):
        status, reason = self.status(lambda run: run["reconciliation"][0].update(invoice=None))
        self.assertEqual(status, "Not reconciled")
        self.assertIn("no invoice figure", reason)

    def test_no_reconciliation_at_all(self):
        self.assertEqual(self.status(lambda run: run.update(reconciliation=[]))[0], "Not reconciled")

    def test_open_period_with_no_invoice(self):
        self.assertEqual(self.status(lambda run: run["reconciliation"][0].update(closed=False, invoice=None))[0], "Open period")

    def test_outside_tolerance_beats_an_open_period(self):
        def change(run):
            run["figures"]["oct_invoice"]["value"] = "57100.00"
            run["reconciliation"][1].update(closed=False)

        self.assertEqual(self.status(change)[0], "Not reconciled")

    def test_partial_scope_with_no_invoice(self):
        def change(run):
            run["scope"]["scope_kind"] = "partial"
            for entry in run["reconciliation"]:
                entry["invoice"] = None

        self.assertEqual(self.status(change)[0], "Partial scope")

    def test_part_month_window_is_partial_even_if_called_whole_bill(self):
        self.assertEqual(self.status(lambda run: run["scope"].update(period="2026-10-01_2026-10-07"))[0], "Partial scope")

    def test_open_period_beats_partial_scope(self):
        def change(run):
            run["scope"]["scope_kind"] = "partial"
            run["reconciliation"][0]["closed"] = False

        self.assertEqual(self.status(change)[0], "Open period")

    def test_cross_checked_with_a_second_view(self):
        def change(run):
            run["reconciliation"][1].update(invoice=None, second_view="sep_query_total")

        self.assertEqual(self.status(change)[0], "Cross-checked only")

    def test_second_view_outside_tolerance(self):
        def change(run):
            run["figures"]["other_view"] = {**run["figures"]["sep_query_total"], "value": "50000"}
            run["reconciliation"][1].update(invoice=None, second_view="other_view")

        self.assertEqual(self.status(change)[0], "Not reconciled")


class Cli(ValidateCase):
    def test_pass(self):
        done = run_script("finops_validate.py", self.run_dir)
        self.assertEqual(done.returncode, 0, done.stdout)
        self.assertEqual(done.stdout.strip(), "PASS")

    def test_fail_lines_and_exit_1(self):
        self.edit(lambda run: self.figure(run, "oct_query_total").update(value="1.00"))
        done = run_script("finops_validate.py", self.run_dir)
        self.assertEqual(done.returncode, 1)
        self.assertRegex(done.stdout, r"(?m)^FAIL FIGURE_MISMATCH \S+: ")

    def test_no_arguments_exits_2(self):
        self.assertEqual(run_script("finops_validate.py").returncode, 2)


if __name__ == "__main__":
    unittest.main()
