"""Shared helpers for the finops unit and workflow tests."""
from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
SCRIPTS = REPO / "plugins/sre-agent/skills/finops/scripts"
FIXTURES = REPO / "tests/fixtures/finops"

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
