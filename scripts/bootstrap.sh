#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"

SHFMT_VERSION="v3.13.1"
ACTIONLINT_VERSION="v1.7.7"
GITLEAKS_VERSION="v8.21.2"
SHELLCHECK_VERSION="v0.11.0"
DUCKDB_VERSION="v2.0.0"

usage() {
  cat <<'EOF'
Usage: scripts/bootstrap.sh [--ci]

Install developer tooling used by the scripts/ check suite:
  shellcheck shfmt yamllint actionlint prettier markdownlint-cli2
  gitleaks pre-commit duckdb (and git-cliff on macOS for local release dry-runs).

Version-sensitive tools (shfmt, actionlint, gitleaks) are installed via
`go install` at pinned versions on every platform so local output matches CI.
On CI, shellcheck is installed from the pinned upstream release (the apt
package lags and enables different default checks, e.g. SC2015). DuckDB is
installed from its pinned release everywhere; only the finops SQL tests use it.

Options:
  --ci        Non-interactive install for Linux CI runners (apt/go/npm/pip).
  -h, --help  Show this help.

With no flag, installs via Homebrew on macOS.
EOF
}

# Install the pinned, version-sensitive Go tools. Run identically on macOS and
# CI so shfmt/actionlint/gitleaks produce the same results everywhere.
install_go_tools() {
  require_tool go "Install Go (https://go.dev/dl) — required for pinned tooling"
  go install "mvdan.cc/sh/v3/cmd/shfmt@${SHFMT_VERSION}"
  go install "github.com/rhysd/actionlint/cmd/actionlint@${ACTIONLINT_VERSION}"
  # The module path is still declared as zricethezav/gitleaks at this version.
  go install "github.com/zricethezav/gitleaks/v8@${GITLEAKS_VERSION}"
}

# The finops SQL tests compare exact totals, so local runs and CI use one engine
# version. Test-only: the shipped skill never needs DuckDB at runtime.
install_duckdb_pinned() {
  local asset tmp
  case "$(uname -s)" in
    Darwin) asset="duckdb-cli-osx-universal.tar.gz" ;;
    *) asset="duckdb-cli-linux-amd64.tar.gz" ;;
  esac
  tmp="$(mktemp -d)"
  curl -fsSL "https://github.com/duckdb/duckdb/releases/download/${DUCKDB_VERSION}/${asset}" | tar -xz -C "$tmp"
  mkdir -p "$HOME/.local/bin"
  install -m 0755 "$tmp/duckdb" "$HOME/.local/bin/duckdb"
  rm -rf "$tmp"
  "$HOME/.local/bin/duckdb" --version
}

bootstrap_macos() {
  require_tool brew "Install Homebrew from https://brew.sh"
  # Non-pinned tools via brew; version-sensitive ones via go (pinned) below.
  brew install \
    shellcheck yamllint \
    prettier markdownlint-cli2 git-cliff pre-commit
  install_go_tools

  gobin="$(go env GOPATH)/bin"
  if [[ ":$PATH:" != *":$gobin:"* ]]; then
    log_warn "add '$gobin' to your PATH (and ahead of Homebrew) so pinned tools win"
  fi

  install_duckdb_pinned
  if [[ ":$PATH:" != *":$HOME/.local/bin:"* ]]; then
    log_warn "add '$HOME/.local/bin' to your PATH so the finops SQL tests find duckdb"
  fi
}

# ShellCheck is version-sensitive (default-enabled checks move between
# releases: apt's 0.9 emits SC2015, 0.11 does not), so CI installs the same
# pinned upstream release developers get from Homebrew instead of the apt
# package. ~/.local/bin is prepended via GITHUB_PATH, so it wins over the
# runner image's /usr/bin/shellcheck in later steps.
install_shellcheck_pinned() {
  local url="https://github.com/koalaman/shellcheck/releases/download/${SHELLCHECK_VERSION}/shellcheck-${SHELLCHECK_VERSION}.linux.x86_64.tar.xz"
  curl -fsSL "$url" | tar -xJ -C /tmp
  mkdir -p "$HOME/.local/bin"
  install -m 0755 "/tmp/shellcheck-${SHELLCHECK_VERSION}/shellcheck" "$HOME/.local/bin/shellcheck"
  "$HOME/.local/bin/shellcheck" --version
}

bootstrap_ci() {
  require_tool npm "Node/npm must be available on the CI runner"
  require_tool python3 "Python 3 must be available on the CI runner"

  install_shellcheck_pinned
  install_go_tools
  install_duckdb_pinned

  python3 -m pip install --user --quiet yamllint pre-commit
  npm install -g --no-fund --no-audit prettier markdownlint-cli2

  # Persist tool locations to later workflow steps (git-cliff is provided by
  # the release workflow's action, so it is intentionally not installed here).
  if [[ -n "${GITHUB_PATH:-}" ]]; then
    echo "$(go env GOPATH)/bin" >>"$GITHUB_PATH"
    echo "$HOME/.local/bin" >>"$GITHUB_PATH"
  fi
}

main() {
  case "${1:-}" in
    --ci) bootstrap_ci ;;
    -h | --help)
      usage
      exit 0
      ;;
    "")
      case "$(uname -s)" in
        Darwin) bootstrap_macos ;;
        *)
          log_error "Automated local install supports macOS only; use --ci or install tools manually."
          exit 1
          ;;
      esac
      ;;
    *)
      log_error "unknown argument: $1"
      usage
      exit 2
      ;;
  esac
  log_ok "bootstrap complete"
}

main "$@"
