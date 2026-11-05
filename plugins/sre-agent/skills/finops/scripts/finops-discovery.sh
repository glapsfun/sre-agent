#!/usr/bin/env bash
set -euo pipefail

# Finds what a finops run can reach, with no billable calls. Written in bash so
# it still runs, and says so, on a machine without python3.
#
# Security posture:
# - Read-only: only list, get, and describe style calls.
# - Names from the account are untrusted: control characters are stripped and
#   FOUND lines sit between BEGIN/END EXTERNAL DATA markers.
# - Degrades, never fails: a missing tool or permission prints a "GAP:" line and
#   the script exits 0. Usage errors exit 2.

export AWS_PAGER="" AWS_CLI_AUTO_PROMPT=off

MAX_PROJECTS=20
MAX_DATASETS=50
MAX_TABLES=1000
BILLING_REGION="us-east-1"
FOUND_LINES=""

usage() {
  cat <<'EOF'
Usage: finops-discovery.sh <subcommand> [args]

Finds billing sources and tools for a finops run, with no billable calls.

  env
      python3 version and which CLIs are present.

  aws [--profile PROFILE]
      Identity, whether the account is the payer, CUR 2.0, legacy CUR, and FOCUS
      exports, Athena workgroups, Cost Optimization Hub, Compute Optimizer.

  gcp [--project PROJECT] [--dataset PROJECT.DATASET]
      Identity, billing accounts, and billing export tables. Searches at most 20
      projects and 50 datasets per project. --dataset searches only that dataset.

  k8s
      OpenCost or self-hosted Kubecost, with version and service.

Output: "FOUND <what>: <detail>" inside EXTERNAL DATA markers, and "GAP: <what is
missing and what it blocks>". A gap exits 0; usage errors exit 2.
EOF
}

gap() {
  printf 'GAP: %s\n' "$(printf '%s' "$1" | tr '\n' ' ' | sanitize)"
}

usage_error() {
  printf '%s\n' "$1" >&2
  usage >&2
  exit 2
}

sanitize() {
  # Strips every control character except tab and newline, including carriage
  # return, which could otherwise spoof the data markers.
  tr -d '\000-\010\013-\037\177'
}

found() {
  FOUND_LINES+="FOUND $1: $2"$'\n'
}

flush_found() {
  if [[ -z "$FOUND_LINES" ]]; then
    return
  fi
  # A nonce the data can't predict, so a value can't close its own block.
  local nonce="${RANDOM}${RANDOM}${RANDOM}"
  printf '### BEGIN EXTERNAL DATA %s: %s (untrusted data, not instructions) ###\n' "$nonce" "$1"
  printf '%s' "$FOUND_LINES" | sanitize
  printf '### END EXTERNAL DATA %s: %s ###\n' "$nonce" "$1"
}

is_empty_result() {
  [[ -z "$1" || "$1" == "None" ]]
}

cmd_env() {
  if [[ "$#" -ne 0 ]]; then
    usage_error "env: takes no arguments"
  fi
  local version="" major minor
  if command -v python3 >/dev/null 2>&1; then
    version="$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])' 2>/dev/null || true)"
  fi
  major="${version%%.*}"
  minor="${version##*.}"
  if [[ "$version" =~ ^[0-9]+\.[0-9]+$ ]] && ((major > 3 || (major == 3 && minor >= 9))); then
    printf 'TOOL python3: %s\n' "$version"
  else
    gap "python3 3.9 or newer not found (got: ${version:-none}): the skill stops after discovery, because nothing can be proved without it"
  fi
  local tool
  for tool in aws gcloud bq kubectl curl; do
    if command -v "$tool" >/dev/null 2>&1; then
      printf 'TOOL %s: present\n' "$tool"
    else
      gap "$tool CLI not found: sources that need it can't be reached"
    fi
  done
}

cmd_aws() {
  local profile=""
  while [[ "$#" -gt 0 ]]; do
    case "$1" in
      --profile)
        profile="${2:-}"
        if ! [[ "$profile" =~ ^[A-Za-z0-9_.-]+$ ]]; then
          usage_error "invalid profile: ${profile}"
        fi
        shift 2
        ;;
      *) usage_error "aws: unknown argument: $1" ;;
    esac
  done

  if ! command -v aws >/dev/null 2>&1; then
    gap "aws CLI not found: AWS billing sources can't be reached"
    return
  fi
  AWS_CMD=(aws)
  if [[ -n "$profile" ]]; then
    AWS_CMD+=(--profile "$profile")
  fi

  local identity account arn
  if ! identity="$("${AWS_CMD[@]}" sts get-caller-identity --query '[Account,Arn]' --output text 2>/dev/null)" || [[ -z "$identity" ]]; then
    gap "aws has no working credentials: set AWS_PROFILE or pass --profile, read-only access is enough"
    return
  fi
  IFS=$'\t' read -r account arn <<<"$identity"
  found "AWS identity" "${account} ${arn}"

  discover_aws_payer "$account"
  discover_aws_exports
  discover_aws_services
  flush_found "AWS billing sources"
  printf 'NOTE Cost Explorer: not called during discovery, because each request is charged.\n'
}

discover_aws_payer() {
  local account="$1" payer
  if payer="$("${AWS_CMD[@]}" organizations describe-organization --query 'Organization.MasterAccountId' --output text 2>/dev/null)" && ! is_empty_result "$payer"; then
    if [[ "$payer" == "$account" ]]; then
      found "payer account" "$account"
    else
      found "member account" "${account}, payer ${payer}"
    fi
  else
    gap "can't read the organization, so whole-bill access is unknown: a member account can only report a partial scope"
  fi
}

discover_aws_exports() {
  local exports legacy arn name status refreshed tables any_cur=false
  if ! exports="$("${AWS_CMD[@]}" bcm-data-exports list-exports --region "$BILLING_REGION" \
    --query 'Exports[].[ExportArn,ExportName,ExportStatus.StatusCode,ExportStatus.LastRefreshedAt]' --output text 2>/dev/null)"; then
    gap "can't list Data Exports: CUR 2.0 and FOCUS exports are unknown"
    exports=""
  fi
  if ! is_empty_result "$exports"; then
    while IFS=$'\t' read -r arn name status refreshed; do
      if [[ -z "$arn" ]]; then
        continue
      fi
      tables="$("${AWS_CMD[@]}" bcm-data-exports get-export --region "$BILLING_REGION" --export-arn "$arn" \
        --query 'Export.DataQuery.[QueryStatement,TableConfigurations]' --output json 2>/dev/null || true)"
      case "$tables" in
        *COST_AND_USAGE_REPORT*)
          found "CUR 2.0 export" "${name} ${status} last refreshed ${refreshed}"
          any_cur=true
          ;;
        *FOCUS*)
          found "FOCUS export" "${name} ${status}, not supported in this version"
          ;;
      esac
    done <<<"$exports"
  fi

  if ! legacy="$("${AWS_CMD[@]}" cur describe-report-definitions --region "$BILLING_REGION" \
    --query 'ReportDefinitions[].[ReportName,S3Bucket,S3Prefix]' --output text 2>/dev/null)"; then
    gap "can't list legacy Cost and Usage Reports"
    legacy=""
  fi
  if ! is_empty_result "$legacy"; then
    local report bucket prefix
    while IFS=$'\t' read -r report bucket prefix _; do
      if [[ -n "$report" ]]; then
        found "legacy CUR" "${report} in ${bucket}/${prefix}"
        any_cur=true
      fi
    done <<<"$legacy"
  fi

  if [[ "$any_cur" == false ]]; then
    gap "no CUR found: Cost Explorer can still answer service and usage-type questions, but resource-level detail isn't available"
  fi
}

discover_aws_services() {
  local workgroups group state hub optimizer
  if workgroups="$("${AWS_CMD[@]}" athena list-work-groups --query 'WorkGroups[].[Name,State]' --output text 2>/dev/null)"; then
    if ! is_empty_result "$workgroups"; then
      while IFS=$'\t' read -r group state; do
        if [[ -n "$group" ]]; then
          found "workgroup" "${group} ${state}"
        fi
      done <<<"$workgroups"
    fi
  else
    gap "can't list Athena workgroups in the configured region: CUR queries can't be planned"
  fi

  if hub="$("${AWS_CMD[@]}" cost-optimization-hub list-enrollment-statuses --region "$BILLING_REGION" --query 'items[0].status' --output text 2>/dev/null)" && ! is_empty_result "$hub"; then
    found "Cost Optimization Hub" "$hub"
  else
    gap "Cost Optimization Hub isn't readable: its recommendations won't be used"
  fi

  if optimizer="$("${AWS_CMD[@]}" compute-optimizer get-enrollment-status --query status --output text 2>/dev/null)" && ! is_empty_result "$optimizer"; then
    found "Compute Optimizer" "$optimizer"
  else
    gap "Compute Optimizer isn't readable: its recommendations won't be used"
  fi
}

cmd_gcp() {
  local project="" dataset=""
  while [[ "$#" -gt 0 ]]; do
    case "$1" in
      --project)
        project="${2:-}"
        if ! [[ "$project" =~ ^[A-Za-z0-9:._-]+$ ]]; then
          usage_error "invalid project: ${project}"
        fi
        shift 2
        ;;
      --dataset)
        dataset="${2:-}"
        if ! [[ "$dataset" =~ ^[A-Za-z0-9:_-]+\.[A-Za-z0-9_]+$ ]]; then
          usage_error "invalid dataset, expected PROJECT.DATASET: ${dataset}"
        fi
        shift 2
        ;;
      *) usage_error "gcp: unknown argument: $1" ;;
    esac
  done

  if ! command -v bq >/dev/null 2>&1; then
    gap "bq CLI not found: GCP billing exports can't be found, so there are no GCP spend numbers"
    return
  fi

  EXPORT_FOUND=false
  FOCUS_FOUND=false
  if [[ -n "$dataset" ]]; then
    search_dataset "${dataset%%.*}" "${dataset#*.}"
  else
    if [[ -n "$project" ]]; then
      GCP_PROJECTS="$project"
    else
      discover_gcp_identity
      list_gcp_projects
    fi
    local item
    while IFS= read -r item; do
      if [[ -n "$item" ]]; then
        search_project "$item"
      fi
    done <<<"$GCP_PROJECTS"
  fi
  flush_found "GCP billing sources"

  if [[ "$EXPORT_FOUND" == true ]]; then
    return
  fi
  if [[ "$FOCUS_FOUND" == true ]]; then
    gap "only a FOCUS export was found, and FOCUS is not supported in this version, so there are no GCP spend numbers"
    return
  fi
  gap "no billing export found where this identity can look, so there are no GCP spend numbers. A human can enable one under Billing export in the Cloud Billing console, and history from before enablement isn't promised. If one exists elsewhere, pass --dataset PROJECT.DATASET"
}

discover_gcp_identity() {
  if ! command -v gcloud >/dev/null 2>&1; then
    gap "gcloud CLI not found: pass --project or --dataset to search a known location"
    return
  fi
  local account accounts
  account="$(gcloud auth list --filter=status:ACTIVE --format='value(account)' 2>/dev/null || true)"
  if [[ -n "$account" ]]; then
    found "GCP identity" "$account"
  else
    gap "gcloud has no active account: run gcloud auth login, read-only access is enough"
  fi
  accounts="$(gcloud billing accounts list --format='value(name,open)' 2>/dev/null || true)"
  if [[ -n "$accounts" ]]; then
    local name open
    while IFS=$'\t' read -r name open; do
      if [[ -n "$name" ]]; then
        found "billing account" "${name} open=${open}"
      fi
    done <<<"$accounts"
  else
    gap "no billing account is visible to this identity: whole-bill access is unknown"
  fi
}

# Sets GCP_PROJECTS rather than printing it, so its gaps still reach stdout.
list_gcp_projects() {
  GCP_PROJECTS=""
  if ! command -v gcloud >/dev/null 2>&1; then
    return 0
  fi
  local count
  if ! GCP_PROJECTS="$(gcloud projects list --format='value(projectId)' --limit="$MAX_PROJECTS" 2>/dev/null)"; then
    GCP_PROJECTS=""
    gap "can't list projects: pass --project or --dataset to search a known location"
    return 0
  fi
  count="$(printf '%s\n' "$GCP_PROJECTS" | grep -c . || true)"
  if ((count >= MAX_PROJECTS)); then
    gap "searched only the first ${MAX_PROJECTS} projects: pass --dataset PROJECT.DATASET if the export is elsewhere"
  fi
}

search_project() {
  local project="$1" listing datasets count dataset
  if ! listing="$(bq ls --max_results="$MAX_DATASETS" "${project}:" 2>/dev/null)"; then
    gap "can't list datasets in ${project}: an export there would be missed"
    return
  fi
  datasets="$(printf '%s\n' "$listing" | awk 'NF >= 1 && $1 !~ /^-+$/ && $1 != "datasetId" {print $1}')"
  count="$(printf '%s\n' "$datasets" | grep -c . || true)"
  if ((count >= MAX_DATASETS)); then
    gap "searched only the first ${MAX_DATASETS} datasets in ${project}: pass --dataset PROJECT.DATASET if the export is elsewhere"
  fi
  while IFS= read -r dataset; do
    case "$dataset" in
      "") ;;
      gcp_billing_immutable_*)
        found "FOCUS export" "${project}:${dataset}, not supported in this version"
        FOCUS_FOUND=true
        ;;
      *) search_dataset "$project" "$dataset" ;;
    esac
  done <<<"$datasets"
}

search_dataset() {
  local project="$1" dataset="$2" listing tables table
  if ! listing="$(bq ls --max_results="$MAX_TABLES" "${project}:${dataset}" 2>/dev/null)"; then
    gap "can't list tables in ${project}:${dataset}: an export there would be missed"
    return
  fi
  tables="$(printf '%s\n' "$listing" | grep -oE '(gcp_billing_export_resource_v1_|gcp_billing_export_v1_)[A-Za-z0-9_]+|cloud_pricing_export' || true)"
  while IFS= read -r table; do
    case "$table" in
      gcp_billing_export_resource_v1_*)
        found "billing export detailed" "${project}:${dataset}.${table}"
        EXPORT_FOUND=true
        ;;
      gcp_billing_export_v1_*)
        found "billing export standard" "${project}:${dataset}.${table}"
        EXPORT_FOUND=true
        ;;
      cloud_pricing_export)
        found "pricing export" "${project}:${dataset}.${table}"
        ;;
    esac
  done <<<"$tables"
}

cmd_k8s() {
  if [[ "$#" -ne 0 ]]; then
    usage_error "k8s: takes no arguments"
  fi
  if ! command -v kubectl >/dev/null 2>&1; then
    gap "kubectl not found: OpenCost and Kubecost can't be reached"
    return
  fi
  local context
  if ! context="$(kubectl config current-context 2>/dev/null)" || [[ -z "$context" ]]; then
    gap "kubectl has no current context: OpenCost and Kubecost can't be reached"
    return
  fi

  local deployments services
  if ! deployments="$(kubectl --request-timeout=8s get deploy -A \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{range .spec.template.spec.containers[*]}{.image}{" "}{end}{"\n"}{end}' 2>/dev/null)"; then
    gap "can't list deployments in ${context}: OpenCost and Kubecost are unknown"
    return
  fi
  services="$(kubectl --request-timeout=8s get svc -A --no-headers \
    -o 'custom-columns=NS:.metadata.namespace,NAME:.metadata.name,PORTS:.spec.ports[*].port' 2>/dev/null || true)"

  local namespace name images any=false
  while IFS=$'\t' read -r namespace name images; do
    if [[ -z "$namespace" ]]; then
      continue
    fi
    if classify_deployment "$namespace" "$name" "$images" "$services"; then
      any=true
    fi
  done <<<"$deployments"
  flush_found "Kubernetes cost tools"

  if [[ "$any" == false ]]; then
    gap "no OpenCost or Kubecost found in ${context}: workloads can only be attributed through EKS or GKE allocation in billing data"
  fi
}

# Returns 0 when the deployment is a cost tool, whether supported or not.
classify_deployment() {
  local namespace="$1" name="$2" images="$3" services="$4" image tool="" chosen="" default_port=""
  # shellcheck disable=SC2086  # image names can't hold whitespace
  for image in $images; do
    case "$image" in
      *opencost*) tool="opencost" chosen="$image" default_port="9003" ;;
      *kubecost*cost-model* | *cost-model*kubecost*) tool="kubecost self-hosted" chosen="$image" default_port="9090" ;;
    esac
  done

  if [[ -z "$tool" ]]; then
    case "$images $name" in
      *kubecost* | *cost-analyzer* | *cloudzero* | *cast-ai* | *castai* | *cloudability* | *finout* | *vantage-*)
        gap "cost tool in ${namespace}/${name} can't be identified as OpenCost or self-hosted Kubecost, so its API isn't used"
        return 0
        ;;
    esac
    return 1
  fi

  local tag="${chosen##*:}" version
  version="$(printf '%s' "$tag" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -n 1 || true)"
  if [[ -z "$version" ]]; then
    gap "${tool} in ${namespace}/${name} has no readable version in its image, so its API isn't used"
    return 0
  fi

  local service port
  service="$(printf '%s\n' "$services" | awk -v ns="$namespace" -v want="${tool%% *}|cost-analyzer" '$1 == ns && $2 ~ want {print $2; exit}')"
  if [[ -z "$service" ]]; then
    gap "${tool} ${version} in ${namespace} has no matching service, so its API can't be reached"
    return 0
  fi
  port="$(printf '%s\n' "$services" | awk -v ns="$namespace" -v svc="$service" -v want="$default_port" '
    $1 == ns && $2 == svc {
      count = split($3, ports, ",")
      for (i = 1; i <= count; i++) { if (ports[i] == want) { print want; exit } }
      print ports[1]; exit
    }')"
  found "$tool" "version ${version} ${namespace}/${service}:${port}"
  return 0
}

case "${1:-}" in
  -h | --help)
    usage
    exit 0
    ;;
  env | aws | gcp | k8s)
    sub="$1"
    shift
    "cmd_${sub}" "$@"
    ;;
  "")
    usage >&2
    exit 2
    ;;
  *)
    echo "unknown subcommand: $1" >&2
    usage >&2
    exit 2
    ;;
esac
