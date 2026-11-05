"""Drives the same command sequence SKILL.md describes, through stubbed CLIs."""
from __future__ import annotations

import json
import re
import tempfile
import unittest
from datetime import date
from pathlib import Path

from support import SCRIPTS, Scenario, make_path, run_script

from finops_calc import compute
from finops_record import init_run, load_run, save_run
from finops_validate import input_values

SQL = SCRIPTS.parent / "sql"
FIVE_GB = 5368709120


def invoice_json(amount, currency="USD"):
    return json.dumps({"InvoiceSummaries": [{"Entity": {"InvoicingEntity": "AWS"}, "BillType": "ANNIVERSARY", "BaseCurrencyAmount": {"TotalAmountBeforeTax": amount, "CurrencyCode": currency}}]})


def results_json(rows):
    return json.dumps({"ResultSet": {"Rows": [{"Data": [{"VarCharValue": cell} for cell in row]} for row in rows]}})


class Flow(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp = Path(self._tmp.name)
        self.base = self.tmp / "docs/finops"
        self.scenario = Scenario(self.tmp)
        self.next_query = 0

    def new_run(self, provider="aws", scope="123456789012", kind="whole-bill", period="2026-10", comparison="2026-09", day=date(2026, 11, 5), detail=""):
        self.run_dir = init_run(self.base, provider=provider, billing_scope=scope, scope_kind=kind, scope_detail=detail, period=period, comparison_period=comparison, currency="USD", question="why did spend rise?", today=day)
        return self.run_dir

    def script(self, name, *args, expect=0, **kwargs):
        env = kwargs.pop("env", None) or self.scenario.env()
        done = run_script(name, *args, env=env, **kwargs)
        self.assertEqual(done.returncode, expect, f"{name} {args}\n{done.stdout}\n{done.stderr}")
        return done

    def approve(self, athena=FIVE_GB, bigquery=FIVE_GB, ce=20):
        self.script("finops_record.py", "approve-budget", self.run_dir, "--athena-bytes", athena, "--bigquery-bytes", bigquery, "--ce-requests", ce)

    def evidence_id(self, done):
        return re.search(r"(?m)^EVIDENCE (E\d+) ", done.stdout).group(1)

    def merge(self, **sections):
        path = self.tmp / "fragment.json"
        path.write_text(json.dumps(sections))
        self.script("finops_record.py", "merge", self.run_dir, path)

    def set_data_through(self, day="2026-11-03"):
        self.script("finops_record.py", "set-scope", self.run_dir, "--data-through", day)

    def invoice(self, period, amount):
        year, month = period.split("-")
        self.scenario.on("aws", "invoicing", f"BillingPeriod={{Month={int(month)},Year={year}}}", stdout=invoice_json(amount))
        return self.evidence_id(self.script("finops_aws.py", "invoice", self.run_dir, "--account", "123456789012", "--period", period))

    def athena(self, query, rows, params, scanned=1000, expect=0):
        self.next_query += 1
        query_id = f"qid-{self.next_query}"
        s = self.scenario
        s.on("aws", "athena", "start-query-execution", query, stdout=json.dumps({"QueryExecutionId": query_id}))
        s.on("aws", "athena", "get-query-execution", query_id, stdout=json.dumps({"QueryExecution": {"Status": {"State": "SUCCEEDED"}, "Statistics": {"DataScannedInBytes": scanned}}}))
        s.on("aws", "athena", "get-query-results", query_id, stdout=results_json(rows))
        s.responses[-3]["match"] = ["athena", "start-query-execution"]
        args = []
        for key, value in params.items():
            args += ["--param", f"{key}={value}"]
        return self.script("finops_aws.py", "athena", self.run_dir, "--workgroup", "finops", "--database", "billing", "--query-file", SQL / query, *args, "--basis", "billed", expect=expect)

    def control_total(self, period, amount, **kwargs):
        rows = [("billing_period", "currency", "billed_cost"), (period, "USD", amount)]
        return self.evidence_id(self.athena("aws-cur2-control-total.sql", rows, {"table": "cur", "period": period}, **kwargs))

    def measured_csv(self, evidence, value, where, column="billed_cost", kind="measured", basis="billed", currency="USD"):
        return {"value": value, "kind": kind, "currency": currency, "basis": basis, "evidence": evidence, "locator": {"type": "csv", "where": where, "column": column}}

    def invoice_figure(self, evidence, value):
        return {"value": value, "kind": "measured", "currency": "USD", "basis": "billed", "evidence": evidence, "locator": {"type": "json", "path": ["InvoiceSummaries", 0, "BaseCurrencyAmount", "TotalAmountBeforeTax"]}}

    def add_calc(self, calc_id, name, inputs, kind="calculated", assumptions=()):
        run = load_run(self.run_dir)
        calc = {"id": calc_id, "name": name, "kind": kind, "assumptions": list(assumptions), "inputs": inputs}
        calc["outputs"] = compute(name, input_values(run, calc))
        self.merge(calculations=[calc])

    def reconcile_entry(self, period, invoice, query_total, residual, **extra):
        return {"period": period, "closed": True, "invoice": invoice, "second_view": None, "query_total": query_total, "residual": residual, "no_owner": None, "no_owner_share": None, "effective_total": None, "billed_effective_difference": None, "whole_bill_total": None, "scope_share": None, **extra}

    def reconciled_aws_month(self, oct_total="56938.72"):
        """Two months reconciled to their invoices, the base most flows build on."""
        e1 = self.invoice("2026-10", "56940.00")
        e2 = self.invoice("2026-09", "48210.00")
        e3 = self.control_total("2026-10", oct_total)
        e4 = self.control_total("2026-09", "48209.40")
        self.merge(
            figures={
                "oct_invoice": self.invoice_figure(e1, "56940.00"),
                "sep_invoice": self.invoice_figure(e2, "48210.00"),
                "oct_total": self.measured_csv(e3, oct_total, {"billing_period": "2026-10"}),
                "sep_total": self.measured_csv(e4, "48209.40", {"billing_period": "2026-09"}),
            }
        )
        self.add_calc("C1", "residual", {"control": "oct_invoice", "query_total": "oct_total"})
        self.add_calc("C2", "residual", {"control": "sep_invoice", "query_total": "sep_total"})
        self.merge(reconciliation=[self.reconcile_entry("2026-10", "oct_invoice", "oct_total", "C1.residual"), self.reconcile_entry("2026-09", "sep_invoice", "sep_total", "C2.residual")])

    def report(self, expect=0):
        done = self.script("finops_report.py", self.run_dir, expect=expect)
        return done, (self.run_dir / "report.md").read_text(encoding="utf-8") if expect == 0 else ""

    def assert_money_rows_tagged(self, text):
        for line in text.splitlines():
            if "$" in line and line.startswith("|"):
                self.assertRegex(line, r"\b[ECP]\d+\b", line)


class Aws(Flow):
    def test_aws_with_cur(self):
        self.new_run()
        s = self.scenario
        s.on("aws", "sts", "get-caller-identity", stdout="123456789012\tarn:aws:iam::123456789012:user/sre\n")
        s.on("aws", "organizations", stdout="123456789012\n")
        s.on("aws", "bcm-data-exports", "list-exports", stdout="arn:aws:bcm-data-exports:us-east-1:123456789012:export/c\tcur2\tHEALTHY\t2026-11-03T06:00:00Z\n")
        s.on("aws", "bcm-data-exports", "get-export", stdout='["SELECT a FROM COST_AND_USAGE_REPORT"]')
        s.on("aws", "cur", stdout="")
        s.on("aws", "athena", "list-work-groups", stdout="finops\tENABLED\n")
        s.on("aws", "cost-optimization-hub", "list-enrollment-statuses", stdout="Active\n")
        s.on("aws", "compute-optimizer", "get-enrollment-status", stdout="Active\n")
        done = run_script("finops-discovery.sh", "aws", env=s.env())
        self.assertIn("FOUND CUR 2.0 export: cur2", done.stdout)

        self.approve()
        self.reconciled_aws_month()
        self.set_data_through()
        self.add_calc("C3", "change", {"before": "sep_total", "after": "oct_total"})
        self.merge(changes={"total": "C3.change", "pct": "C3.pct", "drivers": [{"name": "Other", "quantity": None, "rate": None, "change": "C3.change", "cause": None}]})

        finished, text = self.report()
        self.assertIn("STATUS Reconciled to invoice", finished.stdout)
        self.assertIn("Status: Reconciled to invoice (tolerance 0.1%)", text)
        self.assertIn("| Query total | $56,938.72 | $48,209.40 | E3, E4 |", text)
        self.assertIn("## What changed: +$8,729.32 (18.1%) [C3]", text)
        self.assert_money_rows_tagged(text)

    def test_inflated_query_total(self):
        self.new_run()
        self.approve()
        self.reconciled_aws_month(oct_total="60000.00")
        self.set_data_through()
        finished, text = self.report()
        self.assertIn("STATUS Not reconciled", finished.stdout)
        self.assertRegex(text, r"(?m)^Status: Not reconciled \(.*2026-10.*\)$")

    def test_aws_cost_explorer_only(self):
        self.new_run()
        self.script("finops_record.py", "add-gap", self.run_dir, "no CUR found: resource-level detail isn't available")
        self.approve()
        page = json.dumps({"ResultsByTime": [{"Total": {"UnblendedCost": {"Amount": "56938.72", "Unit": "USD"}}}]})
        self.scenario.on("aws", "ce", "get-cost-and-usage", stdout=page)
        done = self.script("finops_aws.py", "ce", self.run_dir, "--start", "2026-10-01", "--end", "2026-11-01", "--granularity", "MONTHLY", "--metric", "UnblendedCost")
        evidence = self.evidence_id(done)
        self.merge(figures={"oct_total": {"value": "56938.72", "kind": "measured", "currency": "USD", "basis": "billed", "evidence": evidence, "locator": {"type": "json", "path": ["pages", 0, "ResultsByTime", 0, "Total", "UnblendedCost", "Amount"]}}})
        self.merge(reconciliation=[self.reconcile_entry("2026-10", None, "oct_total", None)])
        self.set_data_through()
        finished, text = self.report()
        self.assertIn("STATUS Not reconciled", finished.stdout)
        self.assertNotIn("Reconciled to invoice", text)
        self.assertIn("resource-level detail isn't available", text)
        self.assertIn("Cost Explorer 1 of 20 requests", text)

    def test_member_account(self):
        self.new_run(kind="partial", scope="111122223333", detail="member account 111122223333")
        self.approve()
        e1 = self.control_total("2026-10", "12000.00")
        rows = [("billing_period", "currency", "billed_cost"), ("2026-10", "USD", "56938.72")]
        e2 = self.evidence_id(self.athena("aws-cur2-control-total.sql", rows, {"table": "cur_payer", "period": "2026-10"}))
        self.merge(figures={"member_total": self.measured_csv(e1, "12000.00", {"billing_period": "2026-10"}), "whole_total": self.measured_csv(e2, "56938.72", {"billing_period": "2026-10"})})
        self.add_calc("C1", "share", {"part": "member_total", "whole": "whole_total"})
        self.merge(reconciliation=[self.reconcile_entry("2026-10", None, "member_total", None, whole_bill_total="whole_total", scope_share="C1.pct")])
        self.set_data_through()
        finished, text = self.report()
        self.assertIn("STATUS Partial scope", finished.stdout)
        self.assertRegex(text, r"(?m)^Status: Partial scope")
        self.assertIn("Scope: member account 111122223333", text)
        share = next(line for line in text.splitlines() if line.startswith("| Share of whole bill"))
        self.assertIn("21.1%", share)
        self.assertIn("C1", share)
        self.assertNotIn("Reconciled to invoice", text)
        self.assert_money_rows_tagged(text)

    def test_no_billable_before_approval(self):
        self.new_run()
        self.script("finops_aws.py", "athena", self.run_dir, "--workgroup", "finops", "--database", "billing", "--query-file", SQL / "aws-cur2-control-total.sql", "--param", "table=cur", "--param", "period=2026-10", expect=3)
        self.script("finops_aws.py", "ce", self.run_dir, "--start", "2026-10-01", "--end", "2026-11-01", "--granularity", "MONTHLY", "--metric", "UnblendedCost", expect=3)
        self.script("finops_aws.py", "ce-commitment", self.run_dir, "--op", "get-savings-plans-utilization", "--start", "2026-10-01", "--end", "2026-11-01", expect=3)
        self.script("finops_gcp.py", "query", self.run_dir, "--project", "p", "--location", "US", "--query-file", SQL / "gcp-invoice-total.sql", "--param", "table=p.d.t", "--param", "invoice_month=2026-10", expect=3)
        self.assertEqual(self.scenario.calls(), [])

    def test_budget_stop_then_report(self):
        self.new_run()
        self.approve(athena=500)
        e1 = self.invoice("2026-10", "56940.00")
        e2 = self.control_total("2026-10", "56938.72", scanned=600, expect=3)
        refused = self.script("finops_aws.py", "athena", self.run_dir, "--workgroup", "finops", "--database", "billing", "--query-file", SQL / "aws-cur2-control-total.sql", "--param", "table=cur", "--param", "period=2026-09", expect=3)
        self.assertRegex(refused.stdout, r"(?m)^BUDGET: ")
        self.script("finops_record.py", "add-gap", self.run_dir, "budget used up before September was queried, so the comparison is missing")
        self.merge(figures={"oct_invoice": self.invoice_figure(e1, "56940.00"), "oct_total": self.measured_csv(e2, "56938.72", {"billing_period": "2026-10"})})
        self.add_calc("C1", "residual", {"control": "oct_invoice", "query_total": "oct_total"})
        self.merge(reconciliation=[self.reconcile_entry("2026-10", "oct_invoice", "oct_total", "C1.residual")])
        self.set_data_through()
        _, text = self.report()
        self.assertIn("budget used up before September was queried", text)
        self.assertIn("Athena 600 B of 500 B approved", text)
        self.assert_money_rows_tagged(text)

    def test_tampered_figure_blocks_report(self):
        self.new_run()
        self.approve()
        self.reconciled_aws_month()
        self.set_data_through()
        run = load_run(self.run_dir)
        run["figures"]["oct_total"]["value"] = "1.00"
        save_run(self.run_dir, run)
        finished, _ = self.report(expect=1)
        self.assertRegex(finished.stdout, r"(?m)^FAIL FIGURE_MISMATCH ")
        self.assertFalse((self.run_dir / "report.md").exists())

    def test_edited_raw_result_blocks_report(self):
        self.new_run()
        self.approve()
        self.reconciled_aws_month()
        self.set_data_through()
        path = next((self.run_dir / "evidence").glob("E3.csv"))
        path.write_text(path.read_text().replace("56938.72", "1.00"))
        finished, _ = self.report(expect=1)
        self.assertRegex(finished.stdout, r"(?m)^FAIL HASH_MISMATCH ")


class Gcp(Flow):
    def query(self, csv_text):
        s = self.scenario
        s.on("gcloud", "auth", "list", stdout="sre@example.test\n")
        s.on("bq", "query", "--dry_run", stdout=json.dumps({"statistics": {"query": {"totalBytesProcessed": "1000"}}}))
        s.on("bq", "query", "--use_legacy_sql=false", "--max_rows=1000000", stdout=csv_text)
        s.on("bq", "show", "-j", stdout=json.dumps({"statistics": {"query": {"totalBytesBilled": "2000"}}}))
        return self.evidence_id(self.script("finops_gcp.py", "query", self.run_dir, "--project", "proj-a", "--location", "US", "--query-file", SQL / "gcp-invoice-total.sql", "--param", "table=proj-a.billing.gcp_billing_export_v1_0123AB", "--param", "invoice_month=2026-10", "--basis", "billed"))

    def test_gcp_with_export(self):
        self.new_run(provider="gcp", scope="0123AB", comparison=None)
        self.approve()
        evidence = self.query("invoice_month,currency,cost,credits,net_cost\n2026-10,USD,390.30,-35.00,355.30\n")
        self.script("finops_record.py", "provided", self.run_dir, "--description", "October invoice before tax", "--value", "355.30", "--currency", "USD", "--given-by", "owner")
        self.merge(
            figures={
                "oct_invoice": {"value": "355.30", "kind": "provided", "currency": "USD", "basis": "billed", "provided": "P1"},
                "oct_total": self.measured_csv(evidence, "355.30", {"invoice_month": "2026-10"}, column="net_cost"),
            }
        )
        self.add_calc("C1", "residual", {"control": "oct_invoice", "query_total": "oct_total"})
        self.merge(reconciliation=[self.reconcile_entry("2026-10", "oct_invoice", "oct_total", "C1.residual")])
        self.set_data_through()
        finished, text = self.report()
        self.assertIn("STATUS Reconciled to invoice", finished.stdout)
        row = next(line for line in text.splitlines() if line.startswith("| Invoice before tax"))
        self.assertIn("$355.30", row)
        self.assertIn("P1", row)
        self.assertIn("### Provided", text)
        self.assertIn("BigQuery", text)

    def stub_identity(self):
        s = self.scenario
        s.on("gcloud", "auth", "list", stdout="sre@example.test\n")
        s.on("gcloud", "billing", "accounts", "list", stdout="billingAccounts/0123AB\tTrue\n")
        s.on("gcloud", "projects", "list", stdout="proj-a\n")

    def gap_from_discovery(self, done):
        line = next(line for line in done.stdout.splitlines() if line.startswith("GAP:") and "spend numbers" in line)
        self.script("finops_record.py", "add-gap", self.run_dir, line[len("GAP: "):])

    def test_gcp_no_export(self):
        self.new_run(provider="gcp", scope="0123AB", comparison=None)
        self.stub_identity()
        self.scenario.on("bq", "ls", "--max_results=50", stdout="  datasetId\n ---\n  other\n")
        self.scenario.on("bq", "ls", "--max_results=1000", "proj-a:other", stdout="  tableId\n ---\n  t1 TABLE\n")
        done = run_script("finops-discovery.sh", "gcp", env=self.scenario.env())
        self.gap_from_discovery(done)
        finished, text = self.report()
        self.assertIn("STATUS Not reconciled", finished.stdout)
        self.assertIn("no GCP spend numbers", text)
        self.assertNotIn("$", text)

    def test_focus_only(self):
        self.new_run(provider="gcp", scope="0123AB", comparison=None)
        self.stub_identity()
        self.scenario.on("bq", "ls", "--max_results=50", stdout="  datasetId\n ---\n  gcp_billing_immutable_0123AB_US\n")
        done = run_script("finops-discovery.sh", "gcp", env=self.scenario.env())
        self.assertIn("FOUND FOCUS export", done.stdout)
        self.gap_from_discovery(done)
        _, text = self.report()
        self.assertIn("not supported in this version", text)
        self.assertNotIn("$", text)


class Kubernetes(Flow):
    WINDOW = "2026-10-01T00:00:00Z,2026-11-01T00:00:00Z"

    def allocation(self, tool, version, service, port, namespace):
        body = {"code": 200, "data": [{"orders": {"totalCost": 600.00}, "__idle__": {"totalCost": 320.00}}]}
        self.scenario.on("kubectl", "get", "--raw", stdout=json.dumps(body))
        self.scenario.on("kubectl", "config", "current-context", stdout="prod\n")
        return self.evidence_id(self.script("finops_k8s.py", "allocation", self.run_dir, "--tool", tool, "--tool-version", version, "--namespace", namespace, "--service", service, "--port", port, "--window", self.WINDOW, "--aggregate", "namespace", "--include-idle"))

    def ledger(self, evidence, covered):
        rows = [("billing_period", "currency", "node_cost"), ("2026-10", "USD", covered)]
        node = self.evidence_id(self.athena("aws-cur2-control-total.sql", rows, {"table": "cur", "period": "2026-10"}))

        def allocated(name):
            return {"value": "600.00" if name == "orders" else "320.00", "kind": "estimated", "currency": "USD", "basis": "effective", "evidence": evidence, "locator": {"type": "json", "path": ["data", 0, name, "totalCost"]}}

        self.merge(figures={"orders": allocated("orders"), "idle": allocated("__idle__"), "covered": self.measured_csv(node, covered, {"billing_period": "2026-10"}, column="node_cost")})
        self.add_calc("C20", "ledger", {"rows": ["orders", "idle"], "covered": "covered"}, kind="estimated")
        self.merge(kubernetes_ledger={"covered": "covered", "calc": "C20", "distributions": [], "rows": [{"kind": "workload", "name": "namespace orders", "amount": "orders", "distributed": False}, {"kind": "idle", "name": "idle", "amount": "idle", "distributed": False}]})

    def test_opencost_ledger(self):
        self.new_run()
        self.approve()
        self.reconciled_aws_month()
        self.set_data_through()
        evidence = self.allocation("opencost", "1.113.0", "opencost", "9003", "opencost")
        self.assertEqual(load_run(self.run_dir)["evidence"][-1]["kind"], "allocation")
        self.ledger(evidence, "920.00")
        finished, text = self.report()
        self.assertIn("### Kubernetes ledger\n", text)
        row = next(line for line in text.splitlines() if line.startswith("| namespace orders"))
        self.assertIn("$600.00", row)
        self.assertRegex(row, r"E\d+ est")
        self.assertIn("STATUS Reconciled to invoice", finished.stdout)

    def test_kubecost_ledger_unreconciled(self):
        self.new_run()
        self.approve()
        self.reconciled_aws_month()
        self.set_data_through()
        evidence = self.allocation("kubecost", "2.4.1", "kubecost-cost-analyzer", "9090", "kubecost")
        self.assertTrue(any("/proxy/model/allocation?" in token for call in self.scenario.called("kubectl", "get", "--raw") for token in call))
        self.ledger(evidence, "1000.00")
        finished, text = self.report()
        self.assertIn("### Kubernetes ledger (unreconciled)", text)
        self.assertIn("workload figures aren't reconciled", text)
        self.assertIn("STATUS Reconciled to invoice", finished.stdout)

    def test_unidentified_cost_tool(self):
        self.new_run()
        done = self.script("finops_k8s.py", "allocation", self.run_dir, "--tool", "unknown", "--tool-version", "1.0.0", "--namespace", "cost", "--service", "tool", "--port", "8080", "--window", self.WINDOW, "--aggregate", "namespace")
        self.assertRegex(done.stdout, r"(?m)^GAP: ")
        self.assertFalse(self.scenario.called("kubectl"))
        self.assertTrue(load_run(self.run_dir)["gaps"])


class Comparison(Flow):
    def first_run(self, period, comparison, day, total="48209.40"):
        self.new_run(period=period, comparison=comparison, day=day)
        self.approve()
        evidence = self.control_total(period, total)
        self.merge(
            figures={
                "total": self.measured_csv(evidence, total, {"billing_period": period}),
                "o12": {"value": "900.00", "kind": "provided", "currency": "USD", "basis": "avoidable", "provided": "P1"},
            }
        )
        self.script("finops_record.py", "provided", self.run_dir, "--description", "owner estimate", "--value", "900.00", "--currency", "USD", "--given-by", "owner")
        self.merge(
            reconciliation=[self.reconcile_entry(period, None, "total", None)],
            opportunities=[{"id": "O-12", "action": "gp3 migration", "owner": "platform", "resource": "ebs", "type": "other", "monthly": "o12", "assumes": ["Owner estimate"], "reliability": {"text": "No capacity change", "evidence": []}, "peak_evidence": None, "state": "identified", "needs_investigation": False, "overlap_calc": None}],
        )
        self.set_data_through(f"{period}-28")
        self.report()
        return self.run_dir

    def test_second_run_compares(self):
        first = self.first_run("2026-09", "2026-08", date(2026, 10, 5))
        # In SKILL.md's order: carry and ask in step 2, then query, then mark once the date is known.
        self.new_run(period="2026-10", comparison="2026-09", day=date(2026, 11, 5))
        carried = self.script("finops_compare.py", "carry", self.run_dir, "--base", self.base)
        self.assertRegex(carried.stdout, r"(?m)^OPPORTUNITY O-12 state=identified gp3 migration$")
        self.assertRegex(carried.stdout, r"(?m)^NEXT_ID O-13$")
        self.approve()
        self.set_data_through()
        self.script("finops_compare.py", "mark", self.run_dir, "--id", "O-12", "--state", "implemented", "--on", "2026-10-15")

        e1 = self.control_total("2026-10", "56938.72")
        e2 = self.control_total("2026-09", "48250.00")
        self.merge(figures={"oct_total": self.measured_csv(e1, "56938.72", {"billing_period": "2026-10"}), "sep_total": self.measured_csv(e2, "48250.00", {"billing_period": "2026-09"})})
        self.merge(reconciliation=[self.reconcile_entry("2026-10", None, "oct_total", None), self.reconcile_entry("2026-09", None, "sep_total", None)])
        restated = self.script("finops_compare.py", "restate", self.run_dir)
        self.assertRegex(restated.stdout, r"(?m)^RESTATED 2026-09 C\d+$")

        _, text = self.report()
        self.assertIn(f"Last report: {first}/report.md", text)
        row = next(line for line in text.splitlines() if line.startswith("| O-12"))
        self.assertIn("too early to measure", row)
        self.assertIn("not measured", row)
        self.assertIn("$900.00", row)
        self.assertRegex(text, r"The total for 2026-09 differs from the last report.*: \+?\$40\.60 \[C\d+ est\]")
        self.assert_money_rows_tagged(text)

    def test_second_run_measures(self):
        self.first_run("2026-07", "2026-06", date(2026, 8, 5), total="3000.00")
        self.new_run(period="2026-08", comparison="2026-07", day=date(2026, 9, 2))
        self.approve()
        self.set_data_through("2026-09-01")
        self.script("finops_compare.py", "carry", self.run_dir, "--base", self.base)
        marked = self.script("finops_compare.py", "mark", self.run_dir, "--id", "O-12", "--state", "implemented", "--on", "2026-07-15")
        self.assertRegex(marked.stdout, r"(?m)^WINDOW baseline=2026-06 post=2026-08$")

        rows = [("billing_period", "item", "billed_cost"), ("2026-06", "gp3", "3000.00"), ("2026-08", "gp3", "2220.00")]
        evidence = self.evidence_id(self.athena("aws-cur2-drivers.sql", rows, {"table": "cur", "period": "2026-08"}))
        self.merge(figures={"baseline": self.measured_csv(evidence, "3000.00", {"billing_period": "2026-06"}), "post": self.measured_csv(evidence, "2220.00", {"billing_period": "2026-08"})})
        self.add_calc("C1", "realized_savings", {"baseline_cost": "baseline", "post_cost": "post"})
        entry = dict(next(item for item in load_run(self.run_dir)["past_savings"] if item["id"] == "O-12"))
        entry.update(result="C1.saving", status="Landed, below estimate")
        self.merge(past_savings=[entry], reconciliation=[self.reconcile_entry("2026-08", None, "post", None)])

        _, text = self.report()
        row = next(line for line in text.splitlines() if line.startswith("| O-12"))
        self.assertIn("$780.00", row)
        self.assertIn("Landed, below estimate", row)
        self.assertIn("C1", row)
        self.assertIn("3,000.00 - 2,220.00, not normalized for usage", text)


class Degraded(Flow):
    def test_no_python_stops_at_discovery(self):
        done = run_script("finops-discovery.sh", "env", env={"PATH": make_path(self.tmp)})
        self.assertEqual(done.returncode, 0)
        self.assertRegex(done.stdout, r"(?m)^GAP: python3 .*stops after discovery")


if __name__ == "__main__":
    unittest.main()
