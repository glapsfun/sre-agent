#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

python3 - <<'PY'
import json
import os
from pathlib import Path
import unittest

PLUGIN = Path("plugins/sre-agent")
SKILL = PLUGIN / "skills/sre-agent"
PREFIX = "mcp__plugin_sre-agent_sre__"
ROLES = {
    "k8s": ({"Bash", "Read", "Grep", "Glob"}, {"discover", "logs_fold", "timeline"}),
    "metrics": ({"Bash", "Read", "Grep", "WebFetch"}, {"discover", "prom_query"}),
    "logs": ({"Bash", "Read", "Grep", "Glob"}, {"discover", "logs_fold"}),
    "changes": ({"Bash", "Read", "Grep", "Glob"}, {"discover", "timeline"}),
    "traces": ({"Bash", "Read", "Grep", "Glob", "WebFetch"}, {"discover"}),
    "eks": ({"Bash", "Read", "Grep", "Glob"}, {"discover", "logs_fold", "timeline"}),
    "gke": ({"Bash", "Read", "Grep", "Glob"}, {"discover", "logs_fold", "timeline"}),
}


def frontmatter(path):
    return dict(line.split(": ", 1) for line in path.read_text().split("---", 2)[1].strip().splitlines())


class Packaging(unittest.TestCase):
    def check_server(self, filename, variable):
        path = PLUGIN / filename
        self.assertTrue(path.is_file(), f"missing {path}")
        manifest = json.loads(path.read_text())
        self.assertEqual(set(manifest["mcpServers"]), {"sre"})
        server = manifest["mcpServers"]["sre"]
        self.assertEqual(server["command"], variable + "/skills/sre-agent/scripts/sre")
        self.assertEqual(server["args"], ["mcp"])
        # Path existence is a packaging check, not harness variable expansion.
        launcher = PLUGIN / server["command"].removeprefix(variable + "/")
        self.assertTrue(launcher.is_file() and os.access(launcher, os.X_OK), launcher)
        return manifest

    def test_claude_launcher(self):
        self.check_server(".mcp.json", "${CLAUDE_PLUGIN_ROOT}")

    def test_gemini_launcher(self):
        manifest = self.check_server("gemini-extension.json", "${extensionPath}")
        self.assertEqual(manifest["name"], "sre-agent")
        self.assertEqual(manifest["version"], json.loads((PLUGIN / ".claude-plugin/plugin.json").read_text())["version"])

    def test_codex_portable_launcher(self):
        manifest_path = PLUGIN / "plugin.json"
        self.assertTrue(manifest_path.is_file(), f"missing {manifest_path}")
        manifest = json.loads(manifest_path.read_text())
        self.assertEqual(manifest["name"], "sre-agent")
        self.assertEqual(manifest["version"], json.loads((PLUGIN / ".claude-plugin/plugin.json").read_text())["version"])
        config = json.loads((PLUGIN / "mcp.json").read_text())
        self.assertEqual(set(config["mcpServers"]), {"sre"})
        server = config["mcpServers"]["sre"]
        self.assertEqual(server["type"], "stdio")
        self.assertEqual(server["command"], "bash")
        self.assertEqual(server["args"], ["${PLUGIN_ROOT}/skills/sre-agent/scripts/sre", "mcp"])
        self.assertTrue(os.access(SKILL / "scripts/sre", os.X_OK))

    def test_investigator_permissions_and_fallback(self):
        for role, (fallback, reads) in ROLES.items():
            with self.subTest(role=role):
                source = SKILL / f"references/investigators/{role}.md"
                meta = frontmatter(source)
                expected = fallback | {PREFIX + name for name in reads}
                self.assertEqual(set(meta["claude-tools"].split(", ")), expected)
                generated = frontmatter(PLUGIN / "agents" / meta["claude-file"])
                self.assertEqual(set(generated["tools"].split(", ")), expected)
                self.assertIn("guarded Bash fallback", source.read_text())
                self.assertIn("read-only guard blocks a command", source.read_text())
                self.assertIn("SRE_KUBE_CONTEXT=<ctx>", source.read_text())

    def test_hooks_cover_named_mcp_reads_and_bash(self):
        import re
        hooks = json.loads((PLUGIN / "hooks/hooks.json").read_text())["hooks"]
        for phase in ("PreToolUse", "PostToolUse", "PostToolUseFailure"):
            for tool in ("Bash", *(PREFIX + name for _, reads in ROLES.values() for name in reads)):
                self.assertTrue(any(re.fullmatch(entry["matcher"], tool) for entry in hooks[phase]), (phase, tool))


unittest.main()
PY
