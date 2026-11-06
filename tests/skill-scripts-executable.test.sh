#!/usr/bin/env bash
set -euo pipefail

# The skills tell the agent to run their shell helpers directly, so each one has
# to ship with its executable bit.

cd "$(git rev-parse --show-toplevel)"

# shellcheck source=tests/lib.sh
source tests/lib.sh

count=0
while IFS= read -r script; do
  test -x "$script" || fail "$script is not executable"
  count=$((count + 1))
done < <(git ls-files 'plugins/*/skills/*/scripts/*.sh')
[[ $count -gt 0 ]] || fail "found no skill shell scripts to check"

echo "PASS: all $count skill shell scripts are executable"
