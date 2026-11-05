"""Shared helpers for the finops unit and workflow tests."""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
from datetime import date
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
SCRIPTS = REPO / "plugins/sre-agent/skills/finops/scripts"
FIXTURES = REPO / "tests/fixtures/finops"
STUB = Path(__file__).resolve().parent / "stubs/stub_cli.py"
STUB_TOOLS = ("aws", "bq", "gcloud", "kubectl", "curl")
BASE_TOOLS = ("bash", "env", "cat", "grep", "sed", "awk", "tr", "cut", "head", "tail", "sort", "date", "mktemp", "rm", "wc", "dirname")

sys.path.insert(0, str(SCRIPTS))


def run_script(name, *args, env=None, cwd=None, stdin=None):
    """Runs a skill script as a user would, returning the completed process."""
    full_env = dict(os.environ)
    if env:
        full_env.update(env)
    path = SCRIPTS / name
    command = [str(path), *map(str, args)]
    return subprocess.run(
        command,
        capture_output=True,
        text=True,
        env=full_env,
        cwd=cwd,
        input=stdin,
        check=False,
    )


class Scenario:
    """Stubbed CLIs for a test. Unmatched calls exit 99, so nothing passes by accident."""

    def __init__(self, tmp):
        self.dir = Path(tempfile.mkdtemp(prefix="stubs-", dir=tmp))
        self.responses = []
        self.tools = set(STUB_TOOLS)

    def on(self, tool, *match, stdout="", stderr="", exit=0):
        if isinstance(stdout, Path):
            stdout = stdout.read_text(encoding="utf-8")
        self.responses.append({"tool": tool, "match": list(match), "stdout": stdout, "stderr": stderr, "exit": exit})
        self.tools.add(tool)
        return self

    def env(self, path=None):
        (self.dir / "responses.json").write_text(json.dumps(self.responses))
        for tool in self.tools:
            wrapper = self.dir / tool
            wrapper.write_text(f'#!/bin/sh\nexec "{sys.executable}" "{STUB}" {tool} "$@"\n')
            wrapper.chmod(0o755)
        return {"PATH": f"{self.dir}:{path or os.environ['PATH']}", "FINOPS_STUB_DIR": str(self.dir)}

    def _log(self):
        log = self.dir / "calls.jsonl"
        if not log.exists():
            return []
        return [json.loads(line) for line in log.read_text().splitlines()]

    def calls(self):
        return [entry["argv"] for entry in self._log()]

    def stdin_of(self, index):
        return self._log()[index]["stdin"]

    def called(self, *match):
        return [call for call in self.calls() if _contains_in_order(call, match)]


def _contains_in_order(argv, match):
    remaining = iter(argv)
    return all(token in remaining for token in match)


def make_path(tmp, *tools, base=True):
    """A PATH holding only the named real tools, since /usr/bin can hold kubectl or aws."""
    directory = Path(tempfile.mkdtemp(prefix="path-", dir=tmp))
    for tool in (*(BASE_TOOLS if base else ()), *tools):
        real = shutil.which(tool)
        if real:
            (directory / tool).symlink_to(real)
    return str(directory)


# The design's synthetic report: billing_period, item, amount, currency.
E3_ROWS = [
    ("2026-10", "total", "56938.72", "USD"),
    ("2026-09", "total", "48209.40", "USD"),
    ("2026-10", "no_owner", "3210.00", "USD"),
    ("2026-09", "no_owner", "2890.00", "USD"),
    ("2026-09", "nat_gb", "100000", ""),
    ("2026-10", "nat_gb", "200000", ""),
    ("2026-09", "nat_rate", "0.045", "USD"),
    ("2026-10", "nat_rate", "0.045", "USD"),
    ("2026-09", "ec2_hours", "10000", ""),
    ("2026-10", "ec2_hours", "13100", ""),
    ("2026-09", "ec2_rate", "1.00", "USD"),
    ("2026-10", "ec2_rate", "1.00", "USD"),
    ("2026-09", "s3_gb", "20000", ""),
    ("2026-10", "s3_gb", "31300", ""),
    ("2026-09", "s3_rate", "0.10", "USD"),
    ("2026-10", "s3_rate", "0.10", "USD"),
    ("2026-06", "gp3_cost", "3000.00", "USD"),
    ("2026-08", "gp3_cost", "2220.00", "USD"),
    ("2026-10", "snapshot_gb", "16000", ""),
    ("2026-10", "snapshot_rate", "0.05", "USD"),
]


def measured(period, item):
    amount, currency = next((a, c) for p, i, a, c in E3_ROWS if p == period and i == item)
    return {
        "value": amount,
        "kind": "measured",
        "currency": currency or None,
        "basis": "billed",
        "evidence": "E3",
        "locator": {"type": "csv", "where": {"billing_period": period, "item": item}, "column": "amount"},
    }


def _invoice(evidence, amount):
    return {
        "value": amount,
        "kind": "measured",
        "currency": "USD",
        "basis": "billed",
        "evidence": evidence,
        "locator": {"type": "json", "path": ["InvoiceSummaries", 0, "BaseCurrencyAmount", "TotalAmountBeforeTax"]},
    }


def _calc(calc_id, name, inputs, kind="calculated", assumptions=()):
    return {"id": calc_id, "name": name, "kind": kind, "assumptions": list(assumptions), "inputs": inputs}


def build_valid_run(tmp: Path) -> Path:
    """Builds the design's synthetic report through the record API."""
    from finops_calc import compute
    from finops_record import add_evidence, init_run, load_run, merge, save_run
    from finops_validate import input_values

    run_dir = init_run(
        Path(tmp) / "docs/finops",
        provider="aws",
        billing_scope="123456789012",
        scope_kind="whole-bill",
        period="2026-10",
        comparison_period="2026-09",
        currency="USD",
        question="why did spend rise, and what can we do about it?",
        today=date(2026, 11, 5),
    )

    def invoice_json(amount):
        return json.dumps({"InvoiceSummaries": [{"BaseCurrencyAmount": {"TotalAmountBeforeTax": amount, "CurrencyCode": "USD"}}]}).encode()

    e3 = "billing_period,item,amount,currency\n" + "".join(",".join(row) + "\n" for row in E3_ROWS)
    prior = {"figures": {"o12_saving": {"value": "900.00"}}}
    hub = '{"items": [{"resourceId": "search-pool", "estimatedMonthlySavings": 1900.00, "currencyCode": "USD"}]}'
    add_evidence(run_dir, kind="billing", source="AWS Invoicing, October", command="aws invoicing list-invoice-summaries --filter BillingPeriod={Month=10,Year=2026}", raw=invoice_json("56940.00"), ext="json", used="1 call", basis="billed", currency="USD")
    add_evidence(run_dir, kind="billing", source="AWS Invoicing, September", command="aws invoicing list-invoice-summaries --filter BillingPeriod={Month=9,Year=2026}", raw=invoice_json("48210.00"), ext="json", used="1 call", basis="billed", currency="USD")
    add_evidence(run_dir, kind="billing", source="Athena, CUR 2.0", command="aws athena start-query-execution", raw=e3.encode(), ext="csv", query_text="SELECT billing_period, item, amount, currency FROM cur WHERE billing_period IN ('2026-09', '2026-10')", job_id="a1b2c3", used="2.1 GB", basis="billed", currency="USD", rows=len(E3_ROWS))
    add_evidence(run_dir, kind="telemetry", source="CloudWatch NAT bytes", command="aws cloudwatch get-metric-statistics", raw=b'{"Datapoints": []}', ext="json", used="1 call")
    add_evidence(run_dir, kind="change", source="orders-api deploy history", command="kubectl rollout history deploy/orders-api", raw=b"REVISION 14: orders-api v2.14\n", ext="txt")
    add_evidence(run_dir, kind="telemetry", source="Prometheus search pool peak", command="kubectl get --raw /api/v1/query", raw=b'{"peak": "0.41"}', ext="json")
    add_evidence(run_dir, kind="recommendation", source="Cost Optimization Hub", command="aws cost-optimization-hub list-recommendations --region us-east-1", raw=hub.encode(), ext="json", used="1 call", currency="USD")
    add_evidence(run_dir, kind="prior_run", source="Last report's run record", command="finops_compare.py carry", raw=json.dumps(prior).encode(), ext="json")

    figures = {
        "oct_invoice": _invoice("E1", "56940.00"),
        "sep_invoice": _invoice("E2", "48210.00"),
        "oct_query_total": measured("2026-10", "total"),
        "sep_query_total": measured("2026-09", "total"),
        "oct_no_owner": measured("2026-10", "no_owner"),
        "sep_no_owner": measured("2026-09", "no_owner"),
        "gp3_baseline": measured("2026-06", "gp3_cost"),
        "gp3_post": measured("2026-08", "gp3_cost"),
        "e7_search": {
            "value": "1900.00",
            "kind": "estimated",
            "currency": "USD",
            "basis": "avoidable",
            "evidence": "E7",
            "locator": {"type": "json", "path": ["items", 0, "estimatedMonthlySavings"]},
        },
        "prior_O-12_estimate": {
            "value": "900.00",
            "kind": "estimated",
            "currency": "USD",
            "basis": "avoidable",
            "evidence": "E8",
            "locator": {"type": "json", "path": ["figures", "o12_saving", "value"]},
        },
    }
    for item in ("nat_gb", "nat_rate", "ec2_hours", "ec2_rate", "s3_gb", "s3_rate"):
        figures[f"sep_{item}"] = measured("2026-09", item)
        figures[f"oct_{item}"] = measured("2026-10", item)
    figures["snapshot_gb"] = measured("2026-10", "snapshot_gb")
    figures["snapshot_rate"] = measured("2026-10", "snapshot_rate")

    def qty_rate(calc_id, item, rate):
        return _calc(calc_id, "qty_rate", {"q0": f"sep_{item}", "r0": f"sep_{rate}", "q1": f"oct_{item}", "r1": f"oct_{rate}"})

    calculations = [
        _calc("C1", "residual", {"control": "oct_invoice", "query_total": "oct_query_total"}),
        _calc("C2", "residual", {"control": "sep_invoice", "query_total": "sep_query_total"}),
        _calc("C3", "share", {"part": "oct_no_owner", "whole": "oct_query_total"}),
        _calc("C4", "share", {"part": "sep_no_owner", "whole": "sep_query_total"}),
        _calc("C5", "change", {"before": "sep_query_total", "after": "oct_query_total"}),
        qty_rate("C6", "nat_gb", "nat_rate"),
        qty_rate("C7", "ec2_hours", "ec2_rate"),
        qty_rate("C8", "s3_gb", "s3_rate"),
        _calc("C9", "sum", {"terms": ["C6.change", "C7.change", "C8.change"]}),
        _calc("C10", "residual", {"control": "C5.change", "query_total": "C9.total"}),
        _calc("C11", "realized_savings", {"baseline_cost": "gp3_baseline", "post_cost": "gp3_post"}),
        _calc("C12", "product", {"factors": ["oct_nat_gb", {"literal": "0.4", "assumption": "40% of NAT bytes are S3"}, "oct_nat_rate"]}, kind="estimated", assumptions=["40% of NAT bytes are S3"]),
        _calc("C13", "product", {"factors": ["snapshot_gb", "snapshot_rate", {"literal": "0.8", "assumption": "Retention drops 80% of snapshot GB"}]}, kind="estimated", assumptions=["Retention drops 80% of snapshot GB"]),
        _calc("C14", "sum", {"terms": ["C12.total", "e7_search", "C13.total"]}, kind="estimated"),
    ]

    merge(run_dir, {"figures": figures})
    run = load_run(run_dir)
    for calc in calculations:
        calc["outputs"] = compute(calc["name"], input_values(run, calc))
        run["calculations"].append(calc)
    save_run(run_dir, run)

    merge(
        run_dir,
        {
            "reconciliation": [
                {"period": "2026-10", "closed": True, "invoice": "oct_invoice", "second_view": None, "query_total": "oct_query_total", "residual": "C1.residual", "no_owner": "oct_no_owner", "no_owner_share": "C3.pct", "effective_total": None, "billed_effective_difference": None, "whole_bill_total": None, "scope_share": None},
                {"period": "2026-09", "closed": True, "invoice": "sep_invoice", "second_view": None, "query_total": "sep_query_total", "residual": "C2.residual", "no_owner": "sep_no_owner", "no_owner_share": "C4.pct", "effective_total": None, "billed_effective_difference": None, "whole_bill_total": None, "scope_share": None},
            ],
            "gaps": ["No resource IDs in this export, so EC2 growth is explained at usage-type level only."],
            "changes": {
                "total": "C5.change",
                "pct": "C5.pct",
                "drivers": [
                    {"name": "NAT data", "quantity": "C6.quantity", "rate": "C6.rate", "change": "C6.change", "cause": {"text": "orders-api v2.14, Oct 9", "evidence": ["E4", "E5"]}},
                    {"name": "EC2", "quantity": "C7.quantity", "rate": "C7.rate", "change": "C7.change", "cause": None},
                    {"name": "S3", "quantity": "C8.quantity", "rate": "C8.rate", "change": "C8.change", "cause": None},
                    {"name": "Other", "quantity": None, "rate": None, "change": "C10.residual", "cause": None},
                ],
            },
            "past_savings": [
                {"id": "O-12", "action": "gp3 migration", "estimated": "prior_O-12_estimate", "implemented_on": "2026-07-15", "result": "C11.saving", "avoidance": None, "status": "Landed, below estimate"},
            ],
            "opportunities": [
                {"id": "O-21", "action": "S3 gateway endpoint", "owner": "platform", "resource": "nat-gateway", "type": "other", "monthly": "C12.total", "assumes": ["40% of NAT bytes are S3"], "reliability": {"text": "No capacity change", "evidence": []}, "peak_evidence": None, "state": "identified", "needs_investigation": False, "overlap_calc": None},
                {"id": "O-22", "action": "Resize search node pool", "owner": "search", "resource": "search-pool", "type": "rightsizing", "monthly": "e7_search", "assumes": ["Provider estimate"], "reliability": {"text": "Peak 41% over 30d", "evidence": ["E6"]}, "peak_evidence": "E6", "state": "identified", "needs_investigation": False, "overlap_calc": None},
                {"id": "O-23", "action": "Shorten snapshot retention", "owner": "platform", "resource": "snapshots", "type": "other", "monthly": "C13.total", "assumes": ["30-day restore window is enough"], "reliability": {"text": "Needs owner confirmation", "evidence": []}, "peak_evidence": None, "state": "identified", "needs_investigation": False, "overlap_calc": None},
            ],
            "savings_total": "C14.total",
        },
    )

    run = load_run(run_dir)
    run["scope"]["data_through"] = "2026-11-03"
    run["scope"]["last_report"] = "docs/finops/2026-10-05-aws-123456789012-2026-09/report.md"
    run["budget"] = {
        "approved": True,
        "limits": {"athena_bytes": 5368709120, "bigquery_bytes": 0, "ce_requests": 20},
        "used": {"athena_bytes": 2254857830, "bigquery_bytes": 0, "ce_requests": 6},
    }
    save_run(run_dir, run)
    return run_dir


def recompute(run, calc):
    from finops_calc import compute
    from finops_validate import input_values

    calc["outputs"] = compute(calc["name"], input_values(run, calc))


def add_provided_figure(run, name, value, currency="USD"):
    provided_id = f"P{len(run['provided']) + 1}"
    run["provided"].append({"id": provided_id, "description": name, "value": value, "currency": currency, "given_by": "test", "given_at": "2026-11-05T00:00:00Z"})
    run["figures"][name] = {"value": value, "kind": "provided", "currency": currency, "basis": "billed", "provided": provided_id}


def add_ledger(run, covered="1000"):
    """The design's example: $700 direct, $100 shared, $200 idle, the pools split 2:1."""
    for name, value in (("covered", covered), ("direct", "700"), ("shared_pool", "100"), ("idle_pool", "200"), ("pool_total", "300")):
        add_provided_figure(run, name, value)
    distribution = {"id": "C21", "name": "pool_distribution", "kind": "estimated", "assumptions": [], "inputs": {"pool": "pool_total", "weights": [{"literal": "2", "assumption": "policy"}, {"literal": "1", "assumption": "policy"}]}}
    ledger = {"id": "C20", "name": "ledger", "kind": "estimated", "assumptions": [], "inputs": {"rows": ["direct", "C21.share_1", "C21.share_2"], "covered": "covered"}}
    run["calculations"].extend([distribution, ledger])
    recompute(run, distribution)
    recompute(run, ledger)
    run["kubernetes_ledger"] = {
        "covered": "covered",
        "calc": "C20",
        "rows": [
            {"kind": "workload", "name": "direct", "amount": "direct", "distributed": False},
            {"kind": "shared", "name": "shared pool", "amount": "shared_pool", "distributed": True},
            {"kind": "idle", "name": "idle pool", "amount": "idle_pool", "distributed": True},
            {"kind": "workload", "name": "recipient a", "amount": "C21.share_1", "distributed": False},
            {"kind": "workload", "name": "recipient b", "amount": "C21.share_2", "distributed": False},
        ],
        "distributions": [{"pool": "pool_total", "calc": "C21", "from_rows": ["shared_pool", "idle_pool"]}],
    }
