#!/usr/bin/env python3
"""Runs one allowlisted, read-only command and saves its output as evidence.

For telemetry and change history. The allowlist is positive: anything not named
here is refused before it runs. It can't record billing evidence, so it can't be
used to make a measured cost figure.
"""
from __future__ import annotations

import argparse
import json
import re
import shlex
import sys
from pathlib import Path

from finops_record import RecordError, add_evidence, failure_reason, gap, load_run, run_cli, show_result

EXIT_USAGE = 2

ALLOWED = {
    "kubectl": (("get",), ("describe",), ("rollout", "history"), ("top",)),
    "git": (("log",), ("show",)),
    "helm": (("history",),),
    "flux": (("get",),),
    "argocd": (("app", "history"),),
    "gh": (("pr", "list"), ("pr", "view"), ("run", "list"), ("run", "view")),
    "aws": (("cloudwatch", "get-metric-statistics"), ("cloudwatch", "get-metric-data"), ("cloudtrail", "lookup-events")),
    "gcloud": (("logging", "read"),),
}
_SECRET = re.compile(r"\bsecrets?\b", re.IGNORECASE)


def refusal(command: list) -> str | None:
    """Why this command can't run, or None if it can."""
    if not command:
        return "give a command after --"
    tool, arguments = command[0], command[1:]
    for prefix in ALLOWED.get(tool, ()):
        if tuple(arguments[: len(prefix)]) == prefix:
            break
    else:
        return f"{' '.join(command[:3])} isn't on the read-only allowlist"
    if tool == "kubectl" and any(_SECRET.search(argument) for argument in arguments):
        return "kubectl can't read secrets: finops evidence is metadata and metrics only"
    # git's --output writes to any path, which would make this not read-only.
    if tool == "git" and any(argument.startswith("--output") for argument in arguments):
        return "git --output writes a file, so it can't run here"
    return None


def main(argv: list | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Run one allowlisted, read-only command and save its output as evidence. Exit 0 includes a GAP: line, 2 is a usage error or refused command.",
        usage="%(prog)s run_dir --source TEXT --kind {telemetry,change} [--identity X] -- COMMAND...",
    )
    parser.add_argument("run_dir", type=Path)
    parser.add_argument("--source", required=True, help="what this evidence is, shown in the report")
    parser.add_argument("--kind", choices=["telemetry", "change"], required=True)
    parser.add_argument("--identity", default="")
    # Split by hand so the command's own flags can never be read as this script's.
    arguments = list(sys.argv[1:] if argv is None else argv)
    split = arguments.index("--") if "--" in arguments else len(arguments)
    args = parser.parse_args(arguments[:split])
    command = arguments[split + 1 :]

    reason = refusal(command)
    if reason:
        print(reason, file=sys.stderr)
        return EXIT_USAGE
    try:
        load_run(args.run_dir)
        code, out, err = run_cli(command)
        if code:
            gap(failure_reason(" ".join(command[:3]), code, err), args.run_dir)
            return 0
        try:
            json.loads(out)
            ext = "json"
        except ValueError:
            ext = "txt"
        evidence_id = add_evidence(
            args.run_dir,
            kind=args.kind,
            source=args.source,
            command=shlex.join(command),
            raw=out.encode(),
            ext=ext,
            identity=args.identity,
            used="1 call",
        )
        show_result(args.run_dir, evidence_id, args.source, out)
    except RecordError as exc:
        print(str(exc), file=sys.stderr)
        return EXIT_USAGE
    return 0


if __name__ == "__main__":
    sys.exit(main())
