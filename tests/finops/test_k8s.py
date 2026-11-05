from __future__ import annotations

import json
import tempfile
import unittest
from datetime import date
from pathlib import Path

from support import Scenario, make_path, run_script

from finops_record import init_run, load_run, locate

SCRIPT = "finops_k8s.py"
WINDOW = "2026-10-01T00:00:00Z,2026-11-01T00:00:00Z"
ALLOCATION = json.dumps({"code": 200, "data": [{"orders": {"name": "orders", "totalCost": 700.25}, "__idle__": {"name": "__idle__", "totalCost": 200.5}}]})


class K8sCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp = Path(self._tmp.name)
        self.scenario = Scenario(self.tmp)
        self.scenario.on("kubectl", "config", "current-context", stdout="prod\n")
        self.run_dir = init_run(self.tmp / "docs/finops", provider="aws", billing_scope="123456789012", scope_kind="whole-bill", period="2026-10", comparison_period=None, currency="USD", question="q", today=date(2026, 11, 5))

    def allocate(self, tool, version, *extra, namespace="opencost", service="opencost", port="9003", window=WINDOW):
        return run_script(SCRIPT, "allocation", self.run_dir, "--tool", tool, "--tool-version", version, "--namespace", namespace, "--service", service, "--port", port, "--window", window, "--aggregate", "namespace", *extra, env=self.scenario.env())

    def raw_path(self):
        calls = self.scenario.called("kubectl", "get", "--raw")
        self.assertEqual(len(calls), 1, self.scenario.calls())
        return calls[0][calls[0].index("--raw") + 1]

    def evidence(self):
        return load_run(self.run_dir)["evidence"]

    def stub_response(self, stdout=ALLOCATION):
        self.scenario.on("kubectl", "get", "--raw", stdout=stdout)


class Routes(K8sCase):
    def test_opencost_route_and_parameters(self):
        self.stub_response()
        done = self.allocate("opencost", "1.113.0", "--include-idle")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertTrue(self.raw_path().startswith("/api/v1/namespaces/opencost/services/opencost:9003/proxy/allocation?"), self.raw_path())
        for part in (f"window={WINDOW}", "aggregate=namespace", "includeIdle=true", "shareIdle=false"):
            self.assertIn(part, self.raw_path())

    def test_opencost_without_idle(self):
        self.stub_response()
        self.allocate("opencost", "1.113.0")
        self.assertIn("includeIdle=false", self.raw_path())

    def test_kubecost_2_route(self):
        self.stub_response()
        done = self.allocate("kubecost", "2.4.1", namespace="kubecost", service="kubecost-cost-analyzer", port="9090")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertTrue(self.raw_path().startswith("/api/v1/namespaces/kubecost/services/kubecost-cost-analyzer:9090/proxy/model/allocation?"), self.raw_path())
        for part in (f"window={WINDOW}", "aggregate=namespace", "shareIdle=false"):
            self.assertIn(part, self.raw_path())
        self.assertNotIn("includeIdle", self.raw_path())

    def test_kubecost_1_uses_the_same_route(self):
        self.stub_response()
        self.allocate("kubecost", "1.108.0", namespace="kubecost", service="kubecost-cost-analyzer", port="9090")
        self.assertIn("/proxy/model/allocation?", self.raw_path())

    def test_share_idle_flag(self):
        self.stub_response()
        self.allocate("opencost", "1.113.0", "--share-idle")
        self.assertIn("shareIdle=true", self.raw_path())


class Unidentified(K8sCase):
    def assert_gap_and_no_call(self, tool, version):
        done = self.allocate(tool, version)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertRegex(done.stdout, r"(?m)^GAP: ")
        self.assertEqual(self.scenario.calls(), [])
        self.assertEqual(self.evidence(), [])
        self.assertTrue(load_run(self.run_dir)["gaps"])

    def test_kubecost_3_is_unconfirmed(self):
        self.assert_gap_and_no_call("kubecost", "3.0.0")

    def test_kubecost_cloud(self):
        self.assert_gap_and_no_call("kubecost-cloud", "2.4.1")

    def test_unknown_tool(self):
        self.assert_gap_and_no_call("unknown", "1.0.0")

    def test_empty_version(self):
        self.assert_gap_and_no_call("opencost", "")

    def test_unparsable_version(self):
        self.assert_gap_and_no_call("kubecost", "latest")


class Evidence(K8sCase):
    def test_saved_as_allocation_with_numbers_as_returned(self):
        self.stub_response()
        done = self.allocate("opencost", "1.113.0")
        entry = self.evidence()[0]
        self.assertEqual(entry["kind"], "allocation")
        self.assertEqual(entry["identity"], "prod")
        self.assertIn(f"EVIDENCE E1 {entry['raw_path']}", done.stdout)
        path = ["data", 0, "orders", "totalCost"]
        self.assertEqual(locate(self.run_dir / entry["raw_path"], {"type": "json", "path": path}), "700.25")

    def test_bad_response_code_is_a_gap_with_no_evidence(self):
        self.stub_response(json.dumps({"code": 500, "message": "boom", "data": None}))
        done = self.allocate("opencost", "1.113.0")
        self.assertEqual(done.returncode, 0)
        self.assertRegex(done.stdout, r"(?m)^GAP: ")
        self.assertEqual(self.evidence(), [])

    def test_empty_data_is_a_gap_with_no_evidence(self):
        for body in ({"code": 200, "data": []}, {"code": 200, "data": [{}]}, {"code": 200, "data": [None]}):
            scenario = Scenario(self.tmp)
            self.scenario = scenario
            self.stub_response(json.dumps(body))
            done = self.allocate("opencost", "1.113.0")
            with self.subTest(body=body):
                self.assertRegex(done.stdout, r"(?m)^GAP: .*no allocation data")
        self.assertEqual(self.evidence(), [])

    def test_unreadable_response_is_a_gap(self):
        self.stub_response("<html>not json</html>")
        done = self.allocate("opencost", "1.113.0")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertRegex(done.stdout, r"(?m)^GAP: ")
        self.assertEqual(self.evidence(), [])

    def test_failed_call_is_a_gap(self):
        self.scenario.on("kubectl", "get", "--raw", stderr='the server could not find the requested resource', exit=1)
        done = self.allocate("opencost", "1.113.0")
        self.assertEqual(done.returncode, 0)
        self.assertRegex(done.stdout, r"(?m)^GAP: ")

    def test_kubectl_missing_is_a_gap(self):
        done = run_script(SCRIPT, "allocation", self.run_dir, "--tool", "opencost", "--tool-version", "1.113.0", "--namespace", "opencost", "--service", "opencost", "--port", "9003", "--window", WINDOW, "--aggregate", "namespace", env={"PATH": make_path(self.tmp, "python3")})
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertRegex(done.stdout, r"(?m)^GAP: .*kubectl")


class Usage(K8sCase):
    def test_bad_window_exits_2(self):
        done = self.allocate("opencost", "1.113.0", window="x;y")
        self.assertEqual(done.returncode, 2)
        self.assertEqual(self.scenario.calls(), [])

    def test_bad_namespace_and_port_exit_2(self):
        self.assertEqual(self.allocate("opencost", "1.113.0", namespace="a/b").returncode, 2)
        self.assertEqual(self.allocate("opencost", "1.113.0", port="abc").returncode, 2)

    def test_missing_run_directory_exits_2(self):
        self.run_dir = self.tmp / "nope"
        self.assertEqual(self.allocate("opencost", "1.113.0").returncode, 2)


if __name__ == "__main__":
    unittest.main()
