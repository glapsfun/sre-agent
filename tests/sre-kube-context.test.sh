#!/usr/bin/env bash
set -euo pipefail

# SRE_KUBE_CONTEXT pins every cluster call a script makes. Switching the current
# context in another terminal must not redirect a running investigation.

cd "$(git rev-parse --show-toplevel)"

# shellcheck source=tests/lib.sh
source tests/lib.sh

scripts=plugins/sre-agent/skills/sre-agent/scripts

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
export CALL_LOG="$work/calls.log"

cat >"$work/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
printf 'kubectl %s\n' "$*" >>"$CALL_LOG"
case " $* " in
  *" config current-context "*) echo some-other-context ;;
  *" -o json "*) echo '{"items":[]}' ;;
  *" get svc "*) echo 'monitoring prometheus 9090' ;;
  *" get pods "*"-o name "*) echo pod/api-1 ;;
  *" get deploy,statefulset,daemonset "*"-o name "*) echo deployment.apps/api ;;
  *" get deployment "*"-o name "*) echo deployment.apps/api ;;
esac
exit 0
STUB
cat >"$work/bin/crossplane" <<'STUB'
#!/usr/bin/env bash
printf 'crossplane %s\n' "$*" >>"$CALL_LOG"
echo '{}'
STUB
chmod +x "$work/bin/kubectl" "$work/bin/crossplane"

run() {
  PATH="$work/bin:$PATH" SRE_KUBE_CONTEXT="${PIN:-}" "$scripts/$1" "${@:2}" >"$work/out.txt" 2>&1 || true
}

for call in "sre-env-discovery.sh" "sre-obs-discovery.sh" "sre-evidence.sh shop api" \
  "sre-snapshot.sh snapshot shop" "crossplane-status-check.sh" \
  "crossplane-status-check.sh bucket.s3.aws.upbound.io/foo"; do
  read -r -a parts <<<"$call"

  : >"$CALL_LOG"
  PIN=pinned-prod run "${parts[@]}"
  [[ -s "$CALL_LOG" ]] || fail "$call: the stub saw no calls"
  if grep -vE '(^kubectl|^crossplane) .*--context pinned-prod( |$)' "$CALL_LOG"; then
    fail "$call: the calls above aren't pinned"
  fi

  : >"$CALL_LOG"
  PIN='' run "${parts[@]}"
  if grep -E -- '--context' "$CALL_LOG"; then
    fail "$call: unpinned run shouldn't pass --context"
  fi
done

grep -q 'crossplane .*--context' <<<"$(
  : >"$CALL_LOG"
  PIN=pinned-prod run crossplane-status-check.sh bucket.s3.aws.upbound.io/foo
  cat "$CALL_LOG"
)" || fail "the crossplane CLI itself wasn't called"

# --- discovery reports the pinned context, not the current one ----------------
PIN=pinned-prod run sre-env-discovery.sh
grep -q '^context: pinned-prod$' "$work/out.txt" || fail "sre-env-discovery doesn't report the pinned context"
PIN='' run sre-env-discovery.sh
grep -q '^context: some-other-context$' "$work/out.txt" || fail "sre-env-discovery doesn't report the current context"

# --- a port-forward command printed for an agent to run stays pinned too -------
PIN=pinned-prod run sre-obs-discovery.sh
grep -q 'kubectl --context pinned-prod port-forward -n monitoring svc/prometheus' "$work/out.txt" \
  || fail "sre-obs-discovery's port-forward hint isn't pinned"
PIN='' run sre-obs-discovery.sh
grep -q 'port-forward: kubectl port-forward -n monitoring svc/prometheus' "$work/out.txt" \
  || fail "sre-obs-discovery's unpinned port-forward hint changed"

echo "PASS: kube context pin"
