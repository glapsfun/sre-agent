#!/usr/bin/env bash
set -euo pipefail

# Runs the finops Python tests. They live under tests/finops because
# scripts/test.sh only runs tests/*.test.sh.

cd "$(git rev-parse --show-toplevel)"

python3 -m unittest discover -s tests/finops -p 'test_*.py'
echo "PASS: finops unit tests"
