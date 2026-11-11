#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
# shellcheck source=tests/lib.sh
source tests/lib.sh
launcher="$PWD/plugins/sre-agent/skills/sre-agent/scripts/sre"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/releases"
export RELEASE_FIXTURES="$work/releases" DOWNLOAD_LOG="$work/downloads" EXEC_LOG="$work/executions"
export TEST_OS=Linux TEST_ARCH=x86_64 OFFLINE=false
export CLAUDE_PLUGIN_DATA="$work/plugin data" XDG_CACHE_HOME="$work/xdg cache" HOME="$work/home dir"

cat >"$work/bin/uname" <<'STUB'
#!/usr/bin/env bash
case "$1" in -s) echo "$TEST_OS" ;; -m) echo "$TEST_ARCH" ;; *) exit 1 ;; esac
STUB
cat >"$work/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DOWNLOAD_LOG"
if "$OFFLINE"; then exit 6; fi
out='' url='' timeout=false connect=false tls=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) out="$2"; shift ;;
    --max-time) timeout=true; shift ;;
    --connect-timeout) connect=true; shift ;;
    --proto) [[ "$2" == '=https' ]] || exit 98; tls=true; shift ;;
    https://github.com/glapsfun/sre-agent/releases/download/v0.17.0/*) url="$1" ;;
  esac
  shift
done
"$timeout" && "$connect" && "$tls" || exit 97
[[ -n "$out" && -n "$url" ]] || exit 96
cp "$RELEASE_FIXTURES/${url##*/}" "$out"
STUB
chmod +x "$work/bin/uname" "$work/bin/curl"
export PATH="$work/bin:$PATH"

for platform in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64; do
  cat >"$work/releases/sre_v0.17.0_$platform" <<'STUB'
#!/usr/bin/env bash
printf 'executed\n' >>"$EXEC_LOG"
printf '%s\n' "$@"
STUB
done
(cd "$work/releases" && sha256sum sre_v0.17.0_* >SHA256SUMS)

[[ -f "$launcher" ]] || fail "launcher is missing"
output="$("$launcher" version 'argument with spaces')"
[[ "$output" == $'version\nargument with spaces' ]] || fail "arguments changed"
cache="$CLAUDE_PLUGIN_DATA/sre-agent/v0.17.0/linux-amd64"
[[ -f "$cache/sre" ]] || fail "plugin data cache wasn't preferred"
[[ ! -e "$XDG_CACHE_HOME/sre-agent" ]] || fail "wrote second cache"

export OFFLINE=true
before="$(wc -l <"$DOWNLOAD_LOG")"
"$launcher" version >/dev/null
[[ "$(wc -l <"$DOWNLOAD_LOG")" == "$before" ]] || fail "valid cache required network"
printf '\n# modified\n' >>"$cache/sre"
before="$(wc -l <"$EXEC_LOG")"
if "$launcher" version >"$work/out" 2>"$work/err"; then fail "executed tampered cache"; fi
[[ "$(wc -l <"$EXEC_LOG")" == "$before" ]] || fail "tampered cache executed"
grep -qi checksum "$work/err" || fail "missing tamper diagnostic"

export CLAUDE_PLUGIN_DATA="$work/mismatch" OFFLINE=false
printf '\n# tampered download\n' >>"$work/releases/sre_v0.17.0_linux_amd64"
if "$launcher" version >"$work/out" 2>"$work/err"; then fail "accepted checksum mismatch"; fi
[[ "$(wc -l <"$EXEC_LOG")" == "$before" ]] || fail "bad download executed"
[[ ! -e "$CLAUDE_PLUGIN_DATA/sre-agent/v0.17.0/linux-amd64" ]] || fail "published bad cache"
grep -qi checksum "$work/err" || fail "missing checksum diagnostic"

export CLAUDE_PLUGIN_DATA="$work/offline" OFFLINE=true
if "$launcher" version >"$work/out" 2>"$work/err"; then fail "missing cache succeeded offline"; fi
grep -qi 'network\|offline' "$work/err" || fail "missing offline diagnostic"
grep -q 'scripts' "$work/err" || fail "missing script fallback diagnostic"

export CLAUDE_PLUGIN_DATA='' OFFLINE=false TEST_OS=Darwin TEST_ARCH=arm64
"$launcher" version >/dev/null
[[ -f "$XDG_CACHE_HOME/sre-agent/v0.17.0/darwin-arm64/sre" ]] || fail "XDG cache fallback failed"
export XDG_CACHE_HOME='' TEST_ARCH=x86_64
"$launcher" version >/dev/null
[[ -f "$HOME/.cache/sre-agent/v0.17.0/darwin-amd64/sre" ]] || fail "HOME cache fallback failed"
export TEST_OS=Linux TEST_ARCH=aarch64
"$launcher" version >/dev/null
[[ -f "$HOME/.cache/sre-agent/v0.17.0/linux-arm64/sre" ]] || fail "Linux arm64 selection failed"
export TEST_OS=Windows
if "$launcher" version >"$work/out" 2>"$work/err"; then fail "unsupported platform accepted"; fi
grep -qi unsupported "$work/err" || fail "missing unsupported platform diagnostic"

echo 'PASS: sre launcher'
