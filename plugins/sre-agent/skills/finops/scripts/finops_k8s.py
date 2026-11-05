#!/usr/bin/env python3
"""Reads workload allocation from OpenCost or self-hosted Kubecost, read-only.

Goes through the API server's service proxy, so no port-forward is left running.
Figures from here are cost-tool allocations and count as estimated, not measured.
Only versions whose API was confirmed in documentation are called; anything else
is a gap rather than a guess at a route.
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path
from urllib.parse import quote

from finops_record import RecordError, add_evidence, failure_reason, gap, load_run, run_cli, show_result

EXIT_USAGE = 2
KUBECOST_CONFIRMED_MAJORS = {1, 2}
_VERSION = re.compile(r"^(\d+)\.\d+(\.\d+)?")


def _matching(pattern: str, label: str):
    compiled = re.compile(pattern)

    def check(value: str) -> str:
        if not compiled.match(value):
            raise argparse.ArgumentTypeError(f"invalid {label}: {value!r}")
        return value

    return check


_dns_name = _matching(r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?$", "name")
_port = _matching(r"^\d{1,5}$", "port")
_window = _matching(r"^[A-Za-z0-9:.,_-]+$", "window")
_aggregate = _matching(r"^[A-Za-z0-9_:,-]+$", "aggregate")


def _unsupported_reason(tool: str, version: str) -> str | None:
    if tool in {"unknown", "kubecost-cloud"}:
        return f"{tool} isn't a tool with a confirmed allocation API"
    match = _VERSION.match(version)
    if not match:
        return f"{tool} has no readable version ({version or 'empty'})"
    if tool == "kubecost" and int(match.group(1)) not in KUBECOST_CONFIRMED_MAJORS:
        return f"kubecost {version} isn't a version whose API was confirmed (1.x and 2.x were)"
    return None


def _allocation_path(args) -> str:
    idle_name, route = ("includeIdle", "allocation") if args.tool == "opencost" else ("idle", "model/allocation")
    parameters = [
        ("window", args.window),
        ("aggregate", args.aggregate),
        ("accumulate", "true"),
        (idle_name, "true" if args.include_idle else "false"),
        ("shareIdle", "true" if args.share_idle else "false"),
    ]
    query = "&".join(f"{key}={quote(value, safe=':,')}" for key, value in parameters)
    return f"/api/v1/namespaces/{args.namespace}/services/{args.service}:{args.port}/proxy/{route}?{query}"


def _context() -> str:
    code, out, _ = run_cli(["kubectl", "config", "current-context"])
    return out.strip() if code == 0 else ""


def cmd_allocation(args) -> int:
    reason = _unsupported_reason(args.tool, args.tool_version)
    if reason:
        gap(f"{reason}, so its API isn't used and workload attribution relies on billing data", args.run_dir)
        return 0

    path = _allocation_path(args)
    label = f"{args.tool} allocation in {args.namespace}"
    code, out, err = run_cli(["kubectl", "--request-timeout=30s", "get", "--raw", path])
    if code:
        gap(failure_reason(f"kubectl {label}", code, err), args.run_dir)
        return 0
    try:
        body = json.loads(out)
    except ValueError:
        gap(f"{label}: the response wasn't readable, so nothing was recorded", args.run_dir)
        return 0
    if body.get("code") != 200:
        gap(f"{label}: the API returned code {body.get('code')}, so nothing was recorded", args.run_dir)
        return 0
    data = body.get("data") or []
    if not any(data):
        gap(f"{label}: no allocation data for this window", args.run_dir)
        return 0

    evidence_id = add_evidence(
        args.run_dir,
        kind="allocation",
        source=f"{args.tool} {args.tool_version} allocation, {args.window}",
        command=f"kubectl get --raw {path}",
        raw=out.encode(),
        ext="json",
        identity=_context(),
        used="1 call",
        rows=sum(len(item) for item in data if item),
    )
    show_result(args.run_dir, evidence_id, label, out)
    return 0


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Read workload allocation from OpenCost or self-hosted Kubecost. Exit 0 includes a GAP: line, 2 is a usage error.")
    commands = parser.add_subparsers(dest="command", required=True)
    allocation = commands.add_parser("allocation", help="allocation for a window, as evidence")
    allocation.add_argument("run_dir", type=Path)
    allocation.add_argument("--tool", choices=["opencost", "kubecost", "kubecost-cloud", "unknown"], required=True)
    allocation.add_argument("--tool-version", default="")
    allocation.add_argument("--namespace", type=_dns_name, required=True)
    allocation.add_argument("--service", type=_dns_name, required=True)
    allocation.add_argument("--port", type=_port, required=True)
    allocation.add_argument("--window", type=_window, required=True)
    allocation.add_argument("--aggregate", type=_aggregate, required=True)
    allocation.add_argument("--include-idle", action="store_true")
    allocation.add_argument("--share-idle", action="store_true")
    allocation.set_defaults(handler=cmd_allocation)
    return parser


def main(argv: list | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        load_run(args.run_dir)
        return args.handler(args)
    except RecordError as exc:
        print(str(exc), file=sys.stderr)
        return EXIT_USAGE


if __name__ == "__main__":
    sys.exit(main())
