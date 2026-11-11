#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"

usage() {
  cat <<'EOF'
Usage: scripts/check.sh [--all]

Run the developer check suite.
  (default)   Fast suite: fmt --check, lint, validate --fast, Go format/vet/test, launcher tests.
  --all       Everything CI runs: also validate --slow, test, install-test, security.
  -h, --help  Show this help.
EOF
}

all=false
case "${1:-}" in
  --all) all=true ;;
  -h | --help)
    usage
    exit 0
    ;;
  "") ;;
  *)
    log_error "unknown argument: $1"
    usage
    exit 2
    ;;
esac

run() {
  log_info "==> $*"
  "$@"
}

run "$SCRIPT_DIR/fmt.sh" --check
run "$SCRIPT_DIR/lint.sh"
run "$SCRIPT_DIR/validate.sh" --fast
launcher="$REPO_ROOT/plugins/sre-agent/skills/sre-agent/scripts/sre"
if skip_unless_tool shfmt; then
  run shfmt -i 2 -ci -bn -d "$launcher"
fi
if skip_unless_tool shellcheck; then
  run shellcheck "$launcher"
fi
require_tool go 'Go 1.26 or newer is required.'
cd "$REPO_ROOT"
unformatted="$(gofmt -l cmd internal)"
if [[ -n "$unformatted" ]]; then
  log_error "Go formatting issues: $unformatted"
  exit 1
fi
run go vet ./...
run go test ./...
run bash tests/sre-launcher.test.sh

if "$all"; then
  run "$SCRIPT_DIR/validate.sh" --slow
  run "$SCRIPT_DIR/test.sh"
  run "$SCRIPT_DIR/install-test.sh"
  run "$SCRIPT_DIR/security.sh"
fi

log_ok "all checks passed"
