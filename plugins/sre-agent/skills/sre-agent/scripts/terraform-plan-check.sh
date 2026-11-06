#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: terraform-plan-check.sh [--save-plan <file>] <tf-dir> [terraform plan args...]

Read-only Terraform plan classifier for SRE Agent Phase 5/6 remediation.

Runs `terraform plan` in <tf-dir> (already-initialized: this script never
runs `terraform init`), converts the plan to JSON via `terraform show
-json`, and classifies every resource_changes entry into no-op / create /
update / delete / replace (reported as `delete,create`) / forget (a `removed`
block: the resource leaves state and is no longer managed). Extra arguments
(e.g. -var-file=prod.tfvars, -target=..., a workspace already selected via
`terraform workspace select`) are passed through to `terraform plan`
unchanged.

Never mutates infrastructure - `terraform plan` and `terraform show` are
both read-only. The generated plan file is written under a private temp
directory and removed on exit (plan output can contain sensitive values in
cleartext), unless --save-plan asks to keep it.

  --save-plan <file>  Keep the classified plan at <file> (mode 600) and print
                      its absolute path, so the apply can run exactly what was
                      classified: `terraform -chdir=<tf-dir> apply <file>`.
                      Terraform refuses a saved plan once state has moved on.
                      Delete the file after the apply or when the option is
                      dropped. Nothing is kept when classification fails.

Exit codes:
  0  Plan succeeded, no delete, replace, or forget among the changes.
  1  Plan succeeded, but at least one delete, replace, or forget is present -
     treat as high-risk and surface every listed address before approval.
  2  Usage error (missing/invalid <tf-dir> or --save-plan path).
  3  Environment error - terraform/jq missing, <tf-dir> not initialized,
     `terraform plan`/`terraform show` failed, or the plan JSON is empty,
     errored, or not a plan. Output that can't be read is never classified.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

save_plan=""
if [[ "${1:-}" == "--save-plan" ]]; then
  save_plan="${2:-}"
  if [[ -z "$save_plan" ]]; then
    echo "--save-plan requires a file path" >&2
    usage
    exit 2
  fi
  shift 2
  # terraform resolves paths against -chdir, so pin this one to the caller's directory.
  [[ "$save_plan" == /* ]] || save_plan="$PWD/$save_plan"
  if [[ ! -d "$(dirname "$save_plan")" ]]; then
    echo "--save-plan directory does not exist: $(dirname "$save_plan")" >&2
    exit 2
  fi
fi

tf_dir="${1:-}"
if [[ -z "$tf_dir" ]]; then
  echo "terraform-plan-check.sh requires <tf-dir>" >&2
  usage
  exit 2
fi
shift

if [[ ! -d "$tf_dir" ]]; then
  echo "not a directory: $tf_dir" >&2
  exit 2
fi

shopt -s nullglob
tf_files=("$tf_dir"/*.tf)
shopt -u nullglob
if [[ ${#tf_files[@]} -eq 0 ]]; then
  echo "no .tf files found in: $tf_dir" >&2
  exit 2
fi

have() { command -v "$1" >/dev/null 2>&1; }

if ! have terraform; then
  echo "terraform not found on PATH - install it or use the IaC repo's own tooling" >&2
  exit 3
fi

if ! have jq; then
  echo "jq not found on PATH - required to classify plan output" >&2
  exit 3
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
plan_file="$work_dir/plan.tfplan"
json_file="$work_dir/plan.json"

if ! terraform -chdir="$tf_dir" plan -input=false -no-color -out="$plan_file" "$@" >&2; then
  echo "terraform plan failed - see output above" >&2
  exit 3
fi

if ! terraform -chdir="$tf_dir" show -json "$plan_file" >"$json_file"; then
  echo "terraform show -json failed" >&2
  exit 3
fi

# Exactly one JSON document that is a finished plan. Anything else would otherwise
# classify as "no changes" below.
if ! jq -es '
  length == 1
  and (.[0]
    | type == "object"
      and (.format_version | type == "string")
      and .errored != true
      and ((.resource_changes // []) | type == "array"))
' "$json_file" >/dev/null 2>&1; then
  echo "terraform show -json returned no complete plan (empty, errored, or not plan JSON) - refusing to classify it" >&2
  exit 3
fi

if ! summary="$(
  jq -r '
    (.resource_changes // [])
    | map(.change.actions | join(","))
    | group_by(.)
    | map({action: .[0], count: length})
    | sort_by(.action)
    | .[]
    | "\(.action)\t\(.count)"
  ' "$json_file"
)"; then
  echo "failed to parse terraform plan JSON output" >&2
  exit 3
fi

if ! destructive="$(
  jq -r '
    (.resource_changes // [])[]
    | select(.change.actions | any(. == "delete" or . == "forget"))
    | .address
  ' "$json_file"
)"; then
  echo "failed to parse terraform plan JSON output" >&2
  exit 3
fi

if [[ -n "$save_plan" ]]; then
  if ! mv "$plan_file" "$save_plan" || ! chmod 600 "$save_plan"; then
    echo "could not save the plan to $save_plan" >&2
    exit 3
  fi
fi

echo "TERRAFORM PLAN CLASSIFICATION: $tf_dir"
if [[ -n "$summary" ]]; then
  printf '%s\n' "$summary"
else
  printf 'no-op\t0\n'
fi

if [[ -n "$save_plan" ]]; then
  echo
  echo "PLAN FILE: $save_plan"
fi

if [[ -n "$destructive" ]]; then
  echo
  echo "⚠ DESTRUCTIVE CHANGE - delete, replace, or forget present:"
  while IFS= read -r addr; do
    printf '  %s\n' "$addr"
  done <<<"$destructive"
  exit 1
fi

exit 0
