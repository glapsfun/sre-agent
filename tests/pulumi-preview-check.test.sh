#!/usr/bin/env bash
set -euo pipefail

# pulumi-preview-check.sh is the gate before a Pulumi update: output it can't
# read must never be classified as a safe preview. Pulumi itself is stubbed.

cd "$(git rev-parse --show-toplevel)"

# shellcheck source=tests/lib.sh
source tests/lib.sh

s=plugins/sre-agent/skills/sre-agent/scripts/pulumi-preview-check.sh
test -x "$s" || fail "$s not executable"
command -v jq >/dev/null 2>&1 || fail "jq is required for this test"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin" "$tmp/stack"
touch "$tmp/stack/Pulumi.yaml"

cat >"$tmp/bin/pulumi" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >"$STUB_ARGS_LOG"
printf '%s' "$STUB_PREVIEW_JSON"
STUB
chmod +x "$tmp/bin/pulumi"
export STUB_ARGS_LOG="$tmp/args.log"

# check <preview-json> [script args...] sets $rc and $out.
check() {
  local json="$1"
  shift
  rc=0
  out="$(STUB_PREVIEW_JSON="$json" PATH="$tmp/bin:$PATH" "$s" "$@" 2>&1)" || rc=$?
}

urn="urn:pulumi:prod::shop::aws:rds/instance:Instance::main"

# --- classification ---------------------------------------------------------
check "{\"steps\":[{\"op\":\"update\",\"urn\":\"$urn\"}],\"changeSummary\":{\"update\":1}}" "$tmp/stack"
[[ $rc -eq 0 ]] || fail "an update-only preview should exit 0 (got $rc): $out"
grep -q $'^update\t1$' <<<"$out" || fail "an update-only preview should count one update: $out"

check "{\"steps\":[{\"op\":\"delete\",\"urn\":\"$urn\"}],\"changeSummary\":{\"delete\":1}}" "$tmp/stack"
[[ $rc -eq 1 ]] || fail "a delete should exit 1 (got $rc)"
grep -qF "$urn" <<<"$out" || fail "a delete should list the URN: $out"

# --- output it can't read is never a safe preview ---------------------------
check '' "$tmp/stack"
[[ $rc -eq 3 ]] || fail "empty preview output should exit 3 (got $rc): $out"

check '{"diagnostics":[]}' "$tmp/stack"
[[ $rc -eq 3 ]] || fail "JSON without steps and changeSummary should exit 3 (got $rc): $out"

# PULUMI_ENABLE_STREAMING_JSON_PREVIEW turns --json into one engine event per line.
events="{\"sequence\":1,\"resourcePreEvent\":{\"metadata\":{\"op\":\"delete\",\"urn\":\"$urn\"}}}
{\"sequence\":2,\"summaryEvent\":{\"resourceChanges\":{\"delete\":1}}}"
check "$events" "$tmp/stack"
[[ $rc -eq 3 ]] || fail "a streamed event log should exit 3, not pass as safe (got $rc): $out"

# --- extra arguments reach `pulumi preview` ---------------------------------
# references/pulumi-remediation.md relies on this to save the classified plan.
check '{"steps":[],"changeSummary":{}}' "$tmp/stack" "--save-plan=$tmp/plan.json"
[[ $rc -eq 0 ]] || fail "an empty preview should exit 0 (got $rc): $out"
grep -qF -- "--save-plan=$tmp/plan.json" "$STUB_ARGS_LOG" || fail "extra arguments did not reach pulumi preview"

echo "PASS: all pulumi-preview-check assertions hold"
