#!/usr/bin/env bash
set -euo pipefail

# Runs the finops SQL on fixtures in DuckDB. A local run without DuckDB prints
# SKIP and proves nothing about the SQL; on CI a missing DuckDB is a failure.

cd "$(git rev-parse --show-toplevel)"

# shellcheck source=scripts/lib/common.sh
source scripts/lib/common.sh

if ! skip_unless_tool duckdb; then
  echo "SKIP: DuckDB missing, finops SQL not tested"
  exit 0
fi

python3 tests/finops/sql_checks.py
echo "PASS: finops SQL"
