#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X github.com/glapsfun/sre-agent/internal/cli.Version=hook-test" \
  -o "$work/sre" ./cmd/sre

python3 - "$work" <<'PY'
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys

work = Path(sys.argv[1])
root = Path.cwd()
launcher = root / "plugins/sre-agent/skills/sre-agent/scripts/sre"
version = next(line.split("=", 1)[1] for line in launcher.read_text().splitlines() if line.startswith("version="))
os_name = {"Linux": "linux", "Darwin": "darwin"}[platform.system()]
arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
asset = f"sre_{version}_{os_name}_{arch}"
cache = work / "cache"
release = cache / "sre-agent" / version / f"{os_name}-{arch}"
release.mkdir(parents=True)
shutil.copyfile(work / "sre", release / "sre")
(release / "sre").chmod(0o700)
(release / "SHA256SUMS").write_text(f"{hashlib.sha256((release / 'sre').read_bytes()).hexdigest()}  {asset}\n")
env = dict(os.environ, CLAUDE_PLUGIN_DATA=str(cache), CLAUDE_PLUGIN_ROOT=str(root / "plugins/sre-agent"))
subprocess.run(["bash", "tests/sre-readonly-guard.test.sh"], env=env, check=True)
command = json.loads((root / "plugins/sre-agent/hooks/hooks.json").read_text())["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
agent = "sre-agent:sre-k8s-investigator"

def event(command, agent_type=agent):
    value = {"tool_name": "Bash", "tool_input": {"command": command}}
    if agent_type is not None:
        value["agent_type"] = agent_type
    return json.dumps(value)

def expect(label, payload, code, test_env=env, reason=None):
    result = subprocess.run([shutil.which("bash"), "-c", command], input=payload, text=True, capture_output=True, env=test_env)
    assert result.returncode == code, (label, result.returncode, result.stdout, result.stderr)
    assert not result.stdout, (label, result.stdout)
    if reason is not None:
        expected = f"sre-agent read-only guard blocked this: {reason}. Record it under GAPS and move on. Don't look for another way to run it.\n"
        assert result.stderr == expected, (label, result.stderr, expected)
    elif code == 0:
        assert not result.stderr, (label, result.stderr)

expect("wired allowed", event("kubectl get pods"), 0)
expect("wired denied", event("kubectl delete pod x"), 2, reason="kubectl delete pod isn't a read-only command")
expect("main", event("kubectl delete pod x", None), 0)
expect("other", event("kubectl delete pod x", "general-purpose"), 0)
expect("malformed investigator", '{"agent_type":"' + agent + '", broken', 2)
expect("malformed main", '{"tool_input": broken', 0)

# The no-Python success path has every launcher prerequisite and a verified cache.
no_python = work / "no-python"
no_python.mkdir()
for name in ("bash", "cat", "grep", "dirname", "uname", "awk", "sha256sum", "shasum"):
    source = shutil.which(name)
    if source:
        (no_python / name).symlink_to(source)
clean_env = dict(env, PATH=str(no_python))
assert shutil.which("python3", path=clean_env["PATH"]) is None
expect("no Python allowed", event("kubectl get pods"), 0, clean_env)
expect("no Python denied", event("kubectl delete pod x"), 2, clean_env, "kubectl delete pod isn't a read-only command")

# An existing empty cache rejects startup without downloading or touching user data.
missing_root = work / "missing-cache"
(missing_root / "sre-agent" / version / f"{os_name}-{arch}").mkdir(parents=True)
missing_env = dict(env, CLAUDE_PLUGIN_DATA=str(missing_root))
expect("missing checker investigator", event("kubectl get pods"), 2, missing_env)
expect("missing checker main", event("kubectl delete pod x", None), 0, missing_env)
expect("missing checker other", event("kubectl delete pod x", "general-purpose"), 0, missing_env)

failed_root = work / "failed-cache"
failed = failed_root / "sre-agent" / version / f"{os_name}-{arch}"
failed.mkdir(parents=True)
(failed / "sre").write_text("#!/bin/sh\nexit 7\n")
(failed / "sre").chmod(0o700)
(failed / "SHA256SUMS").write_text(f"{hashlib.sha256((failed / 'sre').read_bytes()).hexdigest()}  {asset}\n")
failed_env = dict(env, CLAUDE_PLUGIN_DATA=str(failed_root))
expect("checker failure investigator", event("kubectl get pods"), 2, failed_env)
expect("checker failure main", event("kubectl delete pod x", None), 0, failed_env)
expect("checker failure other", event("kubectl delete pod x", "general-purpose"), 0, failed_env)
print("PASS: Go hook wiring, no-Python runtime, missing checker and runtime failure controls")
PY
