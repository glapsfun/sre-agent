#!/usr/bin/env bash
set -euo pipefail

# Checks the finops skill's documents line up with what ships. Scripts and SQL are
# exercised by the other finops tests; this covers the wiring between them.

cd "$(git rev-parse --show-toplevel)"

# shellcheck source=tests/lib.sh
source tests/lib.sh

skill=plugins/sre-agent/skills/finops
command_file=plugins/sre-agent/commands/finops.md

for file in "$skill/SKILL.md" "$skill/agents/openai.yaml" "$skill/evals/evals.json" "$command_file"; do
  git ls-files --error-unmatch "$file" >/dev/null 2>&1 || fail "$file not git-tracked"
done

# --- SKILL.md names what ships ---------------------------------------------
grep -q '^name: finops$' "$skill/SKILL.md" || fail "SKILL.md frontmatter name isn't finops"
while IFS= read -r path; do
  name="$(basename "$path")"
  grep -qF "$name" "$skill/SKILL.md" || fail "SKILL.md doesn't name $name"
done < <(git ls-files "$skill/scripts" "$skill/references")
grep -qF 'python3' "$skill/SKILL.md" || fail "SKILL.md doesn't state the python3 requirement"
grep -qF 'BEGIN/END EXTERNAL DATA' "$skill/SKILL.md" || fail "SKILL.md doesn't treat query output as untrusted"

# --- every script the docs call exists -------------------------------------
while IFS= read -r reference; do
  [[ -e "$skill/$reference" ]] || fail "docs call $reference, which doesn't exist"
done < <(grep -rhoE 'scripts/finops[-_a-z]*\.(py|sh)|sql/[a-z0-9-]+\.sql' "$skill/SKILL.md" "$skill/references" | sort -u)

# --- the command reaches the skill -----------------------------------------
grep -qF 'finops' "$command_file" || fail "$command_file doesn't name the finops skill"

# --- no em dashes, and no links into gitignored notes ----------------------
while IFS= read -r file; do
  if grep -qP '\x{2014}' "$file"; then
    fail "$file contains an em dash"
  fi
done < <(git ls-files "$skill" "$command_file" tests/finops tests/fixtures/finops 'tests/finops-*.test.sh')
while IFS= read -r file; do
  if grep -qF '.docs/' "$file"; then
    fail "$file refers to .docs/, which is gitignored"
  fi
done < <(git ls-files "$skill" "$command_file")

# --- sre-agent defers cost questions here ----------------------------------
grep -qF "\`finops\`" plugins/sre-agent/skills/sre-agent/references/sibling-skills.md \
  || fail "sibling-skills.md has no finops row"

# --- evals cover the agreed scenarios --------------------------------------
python3 - "$skill/evals/evals.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    data = json.load(handle)

if data.get("skill_name") != "finops":
    sys.exit("evals.json skill_name isn't finops")

evals = data["evals"]
scenarios = {
    "an AWS increase": ["cost explorer", "aws"],
    "a GCP invoice with credits": ["credits", "invoice"],
    "Kubecost against the bill": ["kubecost"],
    "cost per order": ["per order"],
    "combined savings": ["rightsizing", "commitment"],
    "a GCP account with no export": ["no billing export"],
    "a purchase or resize request": ["savings plan"],
    "a forecast request": ["forecast"],
}
for name, keywords in scenarios.items():
    if not any(all(word in item["prompt"].lower() for word in keywords) for item in evals):
        sys.exit(f"no eval covers {name}")

def expected(keyword):
    return next(item["expected_output"].lower() for item in evals if keyword in item["prompt"].lower())

if "isn't supported" not in expected("forecast"):
    sys.exit("the forecast eval doesn't say forecasting isn't supported")
if "declines to apply" not in expected("savings plan"):
    sys.exit("the purchase eval doesn't say the skill declines to apply it")
PY

# --- no script can change anything -----------------------------------------
if git ls-files -z "$skill/scripts" | xargs -0 grep -E 'aws[^|;]*\b(create-|delete-|update-|put-|modify-|terminate-|reboot-|start-instances|stop-|attach-|detach-|associate-|disassociate-|register-|deregister-|tag-|untag-|revoke-|authorize-|run-|reset-|enable-|disable-|cancel-|purge-|add-|remove-|replace-|restore-|copy-|invoke-|publish)'; then
  fail "a script contains a mutating aws verb"
fi
if git ls-files -z "$skill/scripts" | xargs -0 grep -E 'gcloud[^|;]*\b(create|delete|update)\b'; then
  fail "a script contains a mutating gcloud verb"
fi

echo "PASS: finops docs"
