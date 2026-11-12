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

launcher="$(dirname "${BASH_SOURCE[0]}")/../skills/sre-agent/scripts/sre"
[[ -f "$launcher" ]] || block "the packaged checker launcher isn't available"
bash "$launcher" hook pre <<<"$input"
status=$?
case "$status" in
  0 | 2) exit "$status" ;;
  *) block "the guard itself failed with status $status" ;;
esac
