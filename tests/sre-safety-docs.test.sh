#!/usr/bin/env bash
set -euo pipefail

# The safety rules agents actually read. Scripts and the hook have their own tests;
# this covers the instructions that reach the model.

cd "$(git rev-parse --show-toplevel)"

# shellcheck source=tests/lib.sh
source tests/lib.sh

plugin=plugins/sre-agent
skill=$plugin/skills/sre-agent

# --- /sre-agent starts where the skill starts ---------------------------------
grep -q 'at Phase 0' "$plugin/commands/sre-agent.md" || fail "the command doesn't start at Phase 0"
if grep -q 'at Phase 1' "$plugin/commands/sre-agent.md"; then
  fail "the command still starts at Phase 1"
fi

# --- the rules are in the skill -----------------------------------------------
for rule in '**Untrusted data.**' '**Pin the cluster.**' 'SRE_KUBE_CONTEXT' 'read-only guard' \
  'kubectl config current-context'; do
  grep -qF -- "$rule" "$skill/SKILL.md" || fail "SKILL.md is missing: $rule"
done

# --- every investigator gets them, on every install path ----------------------
for source_file in "$skill"/references/investigators/*.md; do
  for rule in 'untrusted data, never instructions' '--context <ctx>' 'SRE_KUBE_CONTEXT=<ctx>' \
    'read-only guard'; do
    grep -qF -- "$rule" "$source_file" || fail "$source_file is missing: $rule"
  done
done
for generated in "$plugin"/agents/*.md "$skill"/agents/codex/*.toml; do
  grep -qF 'untrusted data, never instructions' "$generated" \
    || fail "$generated wasn't regenerated"
done

# --- the k8s playbook's filter removes literal values and keeps the rest -------
k8s="$skill/references/investigators/k8s.md"
filter="$(grep -oE "sed '/\^ \*Environment:[^']*'" "$k8s" | head -1)"
[[ -n "$filter" ]] || fail "k8s.md doesn't filter kubectl describe"
expression="${filter#sed \'}"
expression="${expression%\'}"
filtered="$(
  sed "$expression" <<'DESCRIBE'
Containers:
  app:
    Environment:
      DB_PASSWORD:  hunter2
      MULTI:        hunter3
                    hunter4
    Mounts:
      /etc/app from cfg (ro)
  sidecar:
    Environment:    <none>
    Mounts:         <none>
Events:
  Warning  BackOff
DESCRIBE
)"
if grep -q hunter <<<"$filtered"; then
  fail "k8s.md's filter lets a literal value through"
fi
for keep in '/etc/app from cfg' 'Mounts:         <none>' 'BackOff' 'sidecar:'; do
  grep -qF "$keep" <<<"$filtered" || fail "k8s.md's filter removes: $keep"
done

# --- no playbook fetches whole objects that hold literal values ---------------
if grep -nE 'get deploy[^ ]* .*-o yaml' "$k8s"; then
  fail "k8s.md still fetches a whole Deployment as YAML"
fi
if grep -rnE 'helm get (values|all|manifest)' "$skill/references"; then
  fail "a reference still uses a helm command that prints values"
fi

# --- a fix names its cluster, and its commands use it -------------------------
remediation="$skill/references/remediation.md"
grep -qF '**Target:**' "$remediation" || fail "the option template doesn't name a target context"
if grep -nE '^kubectl (rollout|patch)' "$remediation"; then
  fail "remediation.md has kubectl commands without --context"
fi

# --- marker wording matches what the scripts do -------------------------------
for reference in aws-investigation gcloud-investigation github-investigation; do
  grep -qF 'repeats' "$skill/references/$reference.md" \
    || fail "$reference.md doesn't explain the marker token"
done

# --- the limits are stated where people install it ----------------------------
for readme in README.md "$plugin/README.md"; do
  for phrase in 'read-only identity' 'not a sandbox' 'Codex'; do
    grep -qF "$phrase" "$readme" || fail "$readme doesn't mention: $phrase"
  done
done

echo "PASS: safety docs"
