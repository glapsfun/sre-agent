#!/usr/bin/env bash
set -euo pipefail

# sre-snapshot.sh has to notice a Secret gaining or losing a key, without ever
# reading a Secret value into its output.

cd "$(git rev-parse --show-toplevel)"

# shellcheck source=tests/lib.sh
source tests/lib.sh

s=plugins/sre-agent/skills/sre-agent/scripts/sre-snapshot.sh
test -x "$s" || fail "$s not executable"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"

# The stub hands the script's own output format to a real kubectl when one is
# installed, rendering a client-side dry run that never contacts a cluster, so a
# format kubectl rejects fails here the way it does against a live API server.
REAL_KUBECTL="$(command -v kubectl || true)"
export REAL_KUBECTL

cat >"$tmp/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == --request-timeout=* ]] && shift
[[ "${1:-}" == get ]] || exit 1
shift
case "$1" in
  namespace) exit 0 ;;
  secret)
    echo "secret/db-creds"
    exit 0
    ;;
  secret/db-creds) ;;
  *) exit 0 ;;
esac

format=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == -o ]]; then
    format="$2"
    break
  fi
  shift
done

read -r -a keys <<<"$STUB_SECRET_KEYS"
if [[ -n "$REAL_KUBECTL" ]]; then
  literals=()
  for key in "${keys[@]}"; do
    literals+=("--from-literal=$key=s3cr3t-value")
  done
  KUBECONFIG=/dev/null exec "$REAL_KUBECTL" create secret generic db-creds \
    "${literals[@]}" --dry-run=client -o "$format"
fi
case "$format" in
  go-template=*) printf '%s\n' "${keys[@]}" ;;
  *)
    echo "error: error parsing jsonpath, unrecognized character in action: U+002C ','" >&2
    exit 1
    ;;
esac
STUB
chmod +x "$tmp/bin/kubectl"

snapshot() {
  STUB_SECRET_KEYS="$1" PATH="$tmp/bin:$PATH" "$s" snapshot shop
}

snapshot "alpha" >"$tmp/before.snap"
snapshot "alpha" >"$tmp/same.snap"
snapshot "alpha beta" >"$tmp/after.snap"

grep -q '^K8S secret/db-creds ' "$tmp/before.snap" || fail "snapshot has no line for the Secret"
if grep -q 's3cr3t-value' "$tmp/before.snap" "$tmp/after.snap"; then
  fail "snapshot contains a Secret value"
fi

"$s" diff "$tmp/before.snap" "$tmp/same.snap" >/dev/null || fail "an unchanged Secret is reported as a mutation"

rc=0
out="$("$s" diff "$tmp/before.snap" "$tmp/after.snap")" || rc=$?
[[ $rc -eq 1 ]] || fail "a Secret that gained a key should make diff exit 1 (got $rc)"
grep -q '^CHANGED K8S secret/db-creds ' <<<"$out" || fail "diff does not report the Secret as CHANGED"

echo "PASS: all sre-snapshot assertions hold"
