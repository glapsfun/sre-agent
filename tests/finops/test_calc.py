from __future__ import annotations

import json
import unittest
from datetime import date
from decimal import Decimal as D

from support import run_script

from finops_calc import FORMULAS, CalcError, compute, measure_window, quantize_money, too_early


def out(name, **inputs):
    return compute(name, inputs)


class DesignVectors(unittest.TestCase):
    def test_unit_cost_falls_while_spend_rises(self):
        self.assertEqual(D(out("unit_cost", cost="12000", units="600000")["unit_cost"]), D("0.02"))
        self.assertEqual(D(out("unit_cost", cost="15000", units="1000000")["unit_cost"]), D("0.015"))
        self.assertEqual(D(out("change", before="12000", after="15000")["pct"]), D("25"))
        self.assertEqual(D(out("change", before="0.020", after="0.015")["pct"]), D("-25"))

    def test_combined_unit_cost_is_total_over_total_not_average_of_averages(self):
        cost = out("sum", terms=["100", "900"])["total"]
        units = out("sum", terms=["10", "300"])["total"]
        self.assertEqual(quantize_money(out("unit_cost", cost=cost, units=units)["unit_cost"]), "3.23")

    def test_quantity_and_rate_decomposition(self):
        result = out("qty_rate", q0="100", r0="2", q1="120", r1="2.50")
        self.assertEqual(D(result["quantity"]), D(40))
        self.assertEqual(D(result["rate"]), D(60))
        self.assertEqual(D(result["change"]), D(100))

    def test_overlapping_savings_apply_in_sequence(self):
        result = out("overlap_savings", baseline="10000", reductions=["0.20", "0.30"])
        self.assertEqual(D(result["new_cost"]), D(5600))
        self.assertEqual(D(result["savings"]), D(4400))
        self.assertEqual(D(result["savings_pct"]), D(44))

    def test_net_benefit_payback_and_horizon(self):
        self.assertEqual(D(out("net_benefit", savings="4400", recurring="200")["net"]), D(4200))
        self.assertEqual(quantize_money(out("payback", implementation="6000", net_monthly="4200")["months"]), "1.43")
        total = out("horizon_net", net_monthly="4200", months="12", implementation="6000")["total"]
        self.assertEqual(D(total), D(44400))

    def test_commitment_break_even_and_realized_benefit(self):
        self.assertEqual(D(out("commitment_breakeven", discount="0.30")["utilization"]), D("0.70"))
        result = out("commitment_benefit", on_demand_rate="1.00", discount="0.30", utilization="0.80")
        self.assertEqual(D(result["committed"]), D("0.70"))
        self.assertEqual(D(result["consumed"]), D("0.80"))
        self.assertEqual(D(result["benefit"]), D("0.10"))

    def test_pool_distribution_keeps_the_total(self):
        result = out("pool_distribution", pool="300", weights=["2", "1"])
        self.assertEqual(D(result["share_1"]), D(200))
        self.assertEqual(D(result["share_2"]), D(100))
        self.assertEqual(D(result["total"]), D(300))
        self.assertEqual(D(out("sum", terms=["700", result["share_1"], result["share_2"]])["total"]), D(1000))

    def test_pool_with_a_repeating_share_still_sums_exactly(self):
        result = out("pool_distribution", pool="100", weights=["1", "1", "1"])
        self.assertEqual(D(result["share_1"]) + D(result["share_2"]) + D(result["share_3"]), D(100))
        self.assertEqual(D(result["total"]), D(100))

    def test_realized_savings_normalized_and_not(self):
        result = out("realized_savings", baseline_cost="3000", baseline_usage="1000", post_cost="2664", post_usage="1200")
        self.assertEqual(D(result["saving"]), D(936))
        self.assertEqual(result["normalized"], "true")
        result = out("realized_savings", baseline_cost="3000", post_cost="2220")
        self.assertEqual(D(result["saving"]), D(780))
        self.assertEqual(result["normalized"], "false")

    def test_residual_share_integrate_product_ledger(self):
        self.assertEqual(D(out("residual", control="56940.00", query_total="56938.72")["residual"]), D("1.28"))
        self.assertEqual(D(out("share", part="3210", whole="56938.72")["pct"]).quantize(D("0.1")), D("5.6"))
        self.assertEqual(D(out("integrate", rates=["1", "2"], hours=["10", "5"])["total"]), D(20))
        self.assertEqual(D(out("product", factors=["200000", "0.4", "0.045"])["total"]), D(3600))
        ledger = out("ledger", rows=["700", "100", "150"], covered="1000")
        self.assertEqual(D(ledger["sum"]), D(950))
        self.assertEqual(D(ledger["residual"]), D(50))
        self.assertEqual(D(ledger["residual_pct"]), D(5))


class Undefined(unittest.TestCase):
    def test_zero_denominators_give_undefined(self):
        self.assertEqual(out("unit_cost", cost="10", units="0")["unit_cost"], "undefined")
        self.assertEqual(out("share", part="1", whole="0")["pct"], "undefined")
        self.assertEqual(out("payback", implementation="6000", net_monthly="0")["months"], "undefined")
        self.assertEqual(out("ledger", rows=["1"], covered="0")["residual_pct"], "undefined")
        result = out("realized_savings", baseline_cost="1", baseline_usage="0", post_cost="1", post_usage="1")
        self.assertEqual(result["saving"], "undefined")

    def test_negative_net_never_pays_back(self):
        self.assertEqual(out("payback", implementation="6000", net_monthly="-100")["months"], "undefined")

    def test_change_from_zero_keeps_the_amount(self):
        result = out("change", before="0", after="20")
        self.assertEqual(D(result["change"]), D(20))
        self.assertEqual(result["pct"], "undefined")

    def test_undefined_input_propagates(self):
        self.assertEqual(out("unit_cost", cost="undefined", units="3")["unit_cost"], "undefined")
        self.assertEqual(out("sum", terms=["1", "undefined"])["total"], "undefined")

    def test_negative_baseline_from_a_credit_heavy_month(self):
        result = out("change", before="-50", after="20")
        self.assertEqual(D(result["change"]), D(70))
        self.assertEqual(D(result["pct"]), D(140))


class Refusals(unittest.TestCase):
    def assert_refused(self, name, **inputs):
        with self.assertRaises(CalcError):
            compute(name, inputs)

    def test_float_nan_infinity_and_garbage(self):
        self.assert_refused("residual", control=1.5, query_total="1")
        self.assert_refused("residual", control="NaN", query_total="1")
        self.assert_refused("residual", control="Infinity", query_total="1")
        self.assert_refused("residual", control="abc", query_total="1")

    def test_unknown_name_and_missing_input(self):
        self.assert_refused("forecast", before="1")
        self.assert_refused("residual", control="1")

    def test_integrate_with_unequal_lengths(self):
        self.assert_refused("integrate", rates=["1", "2"], hours=["10"])

    def test_mixed_currencies(self):
        with self.assertRaises(CalcError):
            compute("sum", {"terms": ["1", "2"]}, currencies=["USD", "EUR"])
        self.assertEqual(D(compute("sum", {"terms": ["1", "2"]}, currencies=["USD", "USD"])["total"]), D(3))

    def test_scientific_notation_is_a_number(self):
        self.assertEqual(D(out("residual", control="5.694E4", query_total="56938.72")["residual"]), D("1.28"))

    def test_there_is_no_forecast_calculation(self):
        self.assertFalse([name for name in FORMULAS if "forecast" in name])


class Windows(unittest.TestCase):
    def test_measure_window(self):
        self.assertEqual(measure_window(date(2026, 10, 15)), ("2026-09", "2026-11"))
        self.assertEqual(measure_window(date(2026, 11, 1)), ("2026-10", "2026-12"))
        self.assertEqual(measure_window(date(2026, 1, 20)), ("2025-12", "2026-02"))
        self.assertEqual(measure_window(date(2026, 12, 20)), ("2026-11", "2027-01"))

    def test_too_early_until_the_post_month_is_complete(self):
        self.assertTrue(too_early(date(2026, 10, 15), date(2026, 11, 29)))
        self.assertFalse(too_early(date(2026, 10, 15), date(2026, 11, 30)))


class Cli(unittest.TestCase):
    def test_prints_formula_inputs_and_outputs(self):
        done = run_script("finops_calc.py", "residual", "--input", "control=56940.00", "--input", "query_total=56938.72")
        self.assertEqual(done.returncode, 0, done.stderr)
        payload = json.loads(done.stdout)
        self.assertEqual(D(payload["outputs"]["residual"]), D("1.28"))
        self.assertEqual(payload["inputs"]["control"], "56940.00")
        self.assertIn("control", payload["formula"])

    def test_comma_value_is_a_list(self):
        done = run_script("finops_calc.py", "pool_distribution", "--input", "pool=300", "--input", "weights=2,1")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(D(json.loads(done.stdout)["outputs"]["share_1"]), D(200))

    def test_usage_errors_exit_2(self):
        self.assertEqual(run_script("finops_calc.py", "nope").returncode, 2)
        self.assertEqual(run_script("finops_calc.py", "residual", "--input", "control=abc", "--input", "query_total=1").returncode, 2)
        self.assertEqual(run_script("finops_calc.py").returncode, 2)

    def test_help_exits_0(self):
        done = run_script("finops_calc.py", "--help")
        self.assertEqual(done.returncode, 0)
        self.assertIn("residual", done.stdout)


if __name__ == "__main__":
    unittest.main()
