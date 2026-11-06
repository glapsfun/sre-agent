#!/usr/bin/env bash
# PreToolUse hook for the sre-* investigator subagents. Claude Code lets a tool call
# through on any exit code except 2, so every failure on an investigator's call has
# to end in exit 2. That's also why there's no `set -e` here. Everyone else passes.

input="$(cat)"

investigators='sre-(k8s-investigator|metrics-analyst|logs-investigator|change-historian|trace-analyst|eks-investigator|gke-investigator)'
if ! grep -Eq "\"agent_type\"[[:space:]]*:[[:space:]]*\"([^\"]*:)?${investigators}\"" <<<"$input"; then
  exit 0
fi

block() {
  printf 'sre-agent read-only guard blocked this: %s. Record it under GAPS and move on.\n' "$1" >&2
  exit 2
}

command -v python3 >/dev/null 2>&1 || block "python3 is needed to check commands and isn't available"

# -I keeps a planted module next to the working directory or on PYTHONPATH from loading.
python3 -I "$(dirname "${BASH_SOURCE[0]}")/readonly_guard.py" <<<"$input"
status=$?
case "$status" in
  0 | 2) exit "$status" ;;
  *) block "the guard itself failed with status $status" ;;
esac
