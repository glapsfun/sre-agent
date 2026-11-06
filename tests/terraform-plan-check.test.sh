#!/usr/bin/env bash
set -euo pipefail

# terraform-plan-check.sh is the gate before a Terraform apply: it has to refuse
# what it can't read, flag every destructive action, and be able to hand over the
# exact plan it classified. Terraform itself is stubbed.

cd "$(git rev-parse --show-toplevel)"

# shellcheck source=tests/lib.sh
source tests/lib.sh

s=plugins/sre-agent/skills/sre-agent/scripts/terraform-plan-check.sh
test -x "$s" || fail "$s not executable"
command -v jq >/dev/null 2>&1 || fail "jq is required for this test"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin" "$tmp/tf"
touch "$tmp/tf/main.tf"

# `plan` writes a marker to its -out file and logs the path. `show -json` prints
# whatever $STUB_SHOW_JSON holds.
cat >"$tmp/bin/terraform" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == -chdir=* ]] && shift
case "${1:-}" in
  plan)
    for arg in "$@"; do
      if [[ "$arg" == -out=* ]]; then
        printf 'the classified plan\n' >"${arg#-out=}"
        printf '%s\n' "${arg#-out=}" >"$STUB_OUT_LOG"
      fi
    done
    ;;
  show) printf '%s' "$STUB_SHOW_JSON" ;;
  *) exit 1 ;;
esac
STUB
chmod +x "$tmp/bin/terraform"
export STUB_OUT_LOG="$tmp/out-path.log"

# check <show-json> [script args...] sets $rc and $out.
check() {
  local json="$1"
  shift
  rc=0
  out="$(STUB_SHOW_JSON="$json" PATH="$tmp/bin:$PATH" "$s" "$@" 2>&1)" || rc=$?
}

plan_with() {
  printf '{"format_version":"1.2","resource_changes":[{"address":"%s","change":{"actions":%s}}]}' "$1" "$2"
}

# --- classification ---------------------------------------------------------
check "$(plan_with aws_instance.web '["update"]')" "$tmp/tf"
[[ $rc -eq 0 ]] || fail "an update-only plan should exit 0 (got $rc): $out"
grep -q $'^update\t1$' <<<"$out" || fail "an update-only plan should count one update: $out"

check '{"format_version":"1.2"}' "$tmp/tf"
[[ $rc -eq 0 ]] || fail "a plan with no resource changes should exit 0 (got $rc): $out"

check "$(plan_with aws_db_instance.main '["delete"]')" "$tmp/tf"
[[ $rc -eq 1 ]] || fail "a delete should exit 1 (got $rc)"

check "$(plan_with aws_db_instance.main '["delete","create"]')" "$tmp/tf"
[[ $rc -eq 1 ]] || fail "a replace should exit 1 (got $rc)"

# A `removed` block drops the resource from state: Terraform stops managing it.
check "$(plan_with aws_db_instance.main '["forget"]')" "$tmp/tf"
[[ $rc -eq 1 ]] || fail "a forget should exit 1 (got $rc): $out"
grep -q 'aws_db_instance.main' <<<"$out" || fail "a forget should list the address: $out"

# --- output it can't read is never a safe plan ------------------------------
check '' "$tmp/tf"
[[ $rc -eq 3 ]] || fail "empty plan JSON should exit 3 (got $rc): $out"

check '{"format_version":"1.2","errored":true}' "$tmp/tf"
[[ $rc -eq 3 ]] || fail "an errored plan should exit 3 (got $rc): $out"

check '{"resource_drift":[]}' "$tmp/tf"
[[ $rc -eq 3 ]] || fail "JSON that isn't a plan should exit 3 (got $rc): $out"

check 'not json' "$tmp/tf"
[[ $rc -eq 3 ]] || fail "unparseable plan JSON should exit 3 (got $rc): $out"

# --- the classified plan can be kept for the apply --------------------------
check "$(plan_with aws_instance.web '["update"]')" --save-plan "$tmp/saved.tfplan" "$tmp/tf"
[[ $rc -eq 0 ]] || fail "--save-plan with an update-only plan should exit 0 (got $rc): $out"
[[ "$(cat "$tmp/saved.tfplan" 2>/dev/null)" == "the classified plan" ]] \
  || fail "--save-plan did not keep the plan that was classified"
grep -qF "$tmp/saved.tfplan" <<<"$out" || fail "--save-plan should print where the plan is: $out"

(cd "$tmp" && STUB_SHOW_JSON="$(plan_with aws_instance.web '["update"]')" PATH="$tmp/bin:$PATH" \
  "$OLDPWD/$s" --save-plan relative.tfplan "$tmp/tf" >/dev/null 2>&1) || fail "--save-plan with a relative path failed"
[[ -f "$tmp/relative.tfplan" ]] || fail "a relative --save-plan path should resolve against the caller's directory"

# A plan that failed classification must not be left behind for an apply.
check '' --save-plan "$tmp/unread.tfplan" "$tmp/tf"
[[ $rc -eq 3 ]] || fail "--save-plan with unreadable JSON should exit 3 (got $rc)"
[[ ! -e "$tmp/unread.tfplan" ]] || fail "an unclassified plan was left on disk"

# Without the flag the plan holds cleartext values and must not outlive the run.
check "$(plan_with aws_instance.web '["update"]')" "$tmp/tf"
[[ ! -e "$(cat "$STUB_OUT_LOG")" ]] || fail "the plan file was left on disk without --save-plan"

# --- usage errors -----------------------------------------------------------
expect_exit2 "--save-plan without a path" --save-plan
expect_exit2 "--save-plan without <tf-dir>" --save-plan "$tmp/x.tfplan"
expect_exit2 "missing <tf-dir>"

echo "PASS: all terraform-plan-check assertions hold"
