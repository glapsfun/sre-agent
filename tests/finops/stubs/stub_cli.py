#!/usr/bin/env python3
"""Stands in for aws, bq, gcloud, kubectl, or curl in the finops tests.

Each call is logged. The first unused response whose tool and match tokens fit is
returned; once all fitting responses are used, the last one repeats. A call with
no fitting response exits 99.
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path

directory = Path(os.environ["FINOPS_STUB_DIR"])
tool, args = sys.argv[1], sys.argv[2:]

stdin = sys.stdin.read() if tool == "curl" else ""
with (directory / "calls.jsonl").open("a", encoding="utf-8") as log:
    log.write(json.dumps({"argv": [tool, *args], "stdin": stdin}) + "\n")

responses = json.loads((directory / "responses.json").read_text(encoding="utf-8"))
state_path = directory / "state.json"
used = json.loads(state_path.read_text()) if state_path.exists() else []


def fits(response: dict) -> bool:
    if response["tool"] != tool:
        return False
    remaining = iter(args)
    return all(token in remaining for token in response["match"])


fitting = [index for index, response in enumerate(responses) if fits(response)]
if not fitting:
    print(f"unstubbed call: {tool} {' '.join(args)}", file=sys.stderr)
    sys.exit(99)

unused = [index for index in fitting if index not in used]
chosen = unused[0] if unused else fitting[-1]
if unused:
    state_path.write_text(json.dumps([*used, chosen]))

response = responses[chosen]
sys.stdout.write(response["stdout"])
sys.stderr.write(response["stderr"])
sys.exit(response["exit"])
