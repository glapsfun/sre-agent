# sre-agent

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Agentic SRE orchestrator for [Claude Code](https://docs.anthropic.com/en/docs/claude-code), Codex, Gemini CLI, Copilot CLI, and any agent that supports the [Agent Skills](https://agentskills.io) standard — an assistant that helps human SRE engineers investigate and resolve operational incidents.

Distributed as a single-plugin [Claude Code plugin marketplace](https://docs.anthropic.com/en/docs/claude-code) and as a standard Agent Skill.

## Main goal

Behave like an experienced SRE sitting next to you during an incident. The
agent does **not** replace the human: it drives the investigation — discovers
the environment, collects evidence, finds the most probable root cause,
researches fixes, and proposes remediation options — while **you stay in
control of every change**. Nothing is mutated without your explicit approval,
every apply is preceded by a dry-run, and every option comes with a rollback
plan.

The agent follows a TDD-inspired loop:

1. **Understand** the problem and its scope.
2. **Discover** the environment: cluster, tools, GitOps manager, observability endpoints.
3. **Collect evidence** — Kubernetes state, Prometheus metrics, Loki or Elasticsearch/OpenSearch logs, Tempo/Jaeger traces, service-mesh state, recent changes (git, CI/CD, Helm, Flux/Argo), and cloud-specific evidence on AWS EKS and GCP GKE — via seven read-only investigator playbooks, triaged into a cheaper first wave and an escalation wave, dispatched in parallel as subagents where the host supports them (otherwise inline), with each wave mechanically verified read-only via a state snapshot/diff.
4. **Analyze**: build a timeline, rank root-cause hypotheses, and define *expected behavior* — the measurable criteria a fix must satisfy (the "failing test").
5. **Propose** 2–4 remediation options (description, steps, risk, pros/cons, impact, rollback) and **wait for your approval** — a hard gate.
6. **Apply, validate, iterate**: dry-run → apply → verify every expected-behavior criterion with live evidence — optionally under representative k6 load, behind its own approval gate. Pass → final incident report. Fail → back to step 3 with everything learned retained.

---

## Installation

The full 6-phase investigation works on every target. The difference is only
*how* Phase 3 evidence collection runs: with subagents (parallel, faster) or
inline (sequential, same evidence and format).

### Method 1 — Claude Code (slash commands, recommended)

**Step 1 — Add the marketplace** (one-time per machine):

```
/plugin marketplace add glapsfun/sre-agent
```

This registers the marketplace under the alias **`sre-agent`** from the `name`
field in `.claude-plugin/marketplace.json`.

**Step 2 — Install the plugin**:

```
/plugin install sre-agent@sre-agent
```

To update after a new version is published:

```
/plugin marketplace update sre-agent
```

To remove:

```
/plugin remove sre-agent
```

### Method 2 — Claude Code CLI (non-interactive)

```bash
claude plugin marketplace add glapsfun/sre-agent
claude plugin install sre-agent@sre-agent
```

With npx (no prior global install required):

```bash
npx @anthropic-ai/claude-code plugin marketplace add glapsfun/sre-agent
npx @anthropic-ai/claude-code plugin install sre-agent@sre-agent
```

> Note: `claude "/plugin ..."` (with the slash command as a quoted string)
> passes that string as a model prompt, not as a plugin command — use
> `claude plugin ...` (no leading slash) for non-interactive use.

### Method 3 — Agent Skill (`npx skills`)

Installs the `plugins/sre-agent/skills/sre-agent/` folder into the target
agent's global skills directory:

```bash
npx skills add glapsfun/sre-agent --skill sre-agent --agent codex --global -y
npx skills add glapsfun/sre-agent --skill sre-agent --agent gemini-cli --global -y
npx skills add glapsfun/sre-agent --skill sre-agent --agent copilot --global -y
```

Omit `--global` to install into the current project instead. Verify with
`npx skills list -a codex`, and restart the agent afterwards so the new skill
metadata is loaded.

On Codex, additionally run the skill's bundled
`scripts/install-codex-agents.sh` to install the seven investigator subagents
into `~/.codex/agents/` (with `--project`, run it from your project directory
— the target resolves against your cwd). Re-run it after updating the skill so
the subagents stay in sync with the playbooks.

> **Note:** `npx skills` implements the Agent Skills standard and copies
> **only** the skill folder. The plugin-level `commands/` and `agents/`
> directories are not part of that standard, so the `/sre-agent` slash command
> and the Claude subagents come only from the plugin install (Method 1 or 2).
> The skill itself is fully functional standalone — it runs the complete
> 6-phase investigation inline.

### Method 4 — Local / development install

```bash
git clone https://github.com/glapsfun/sre-agent.git
```

Then, inside Claude Code, using the absolute path to your clone:

```
/plugin marketplace add /path/to/sre-agent
/plugin install sre-agent@sre-agent
```

The path must point to the repo root (the directory containing
`.claude-plugin/marketplace.json`).

---

## Usage

Start an investigation explicitly with the slash command:

```text
/sre-agent my payments service is crash-looping in prod, namespace payments
```

Or just describe the problem — the skill auto-triggers on incident-shaped
requests:

```text
p99 latency on the checkout API went from 200ms to 3s an hour ago. Nothing was deployed.
Our application metrics disappeared from Grafana yesterday.
Pods in namespace search are OOMKilled every few hours.
The checkout service is slow and we run Tempo — find which downstream call is eating the time.
Our logs live in OpenSearch, no Loki — what errors is the orders app throwing since 09:00?
The fix is applied and metrics look good — verify it under load before we close the incident.
```

The agent walks the loop above, keeping a visible **investigation ledger**
(environment, evidence with the command that produced each fact, hypotheses
with confidence, actions, validation results) updated at every phase. When it
reaches the approval gate it presents the options and stops — pick one, ask
for more evidence, or reject them all.

After resolution you get a final incident report (summary, impact, timeline,
root cause, actions, validation, rollback info, follow-ups) you can save to
your repo under `docs/sre-incidents/`.

See the [plugin README](plugins/sre-agent/README.md) for the full usage guide,
the component breakdown, and the degraded-environment behavior.

## Discovery CLI

Run read-only environment discovery with an installed binary, or from this
source checkout with Go 1.26 or newer:

```bash
sre discover --context production --only env,obs --timeout 10s
go run ./cmd/sre discover --context production
```

The default runs environment, observability, AWS, GCP, and GitHub probes in
parallel with a 10-second deadline per probe. JSON retains successful findings
and explicit gaps when another probe fails. `--format text` pretty prints the
report. `SRE_SKIP_CLUSTER=true` skips all subprocesses and reports local tools.
See [discovery](plugins/sre-agent/skills/sre-agent/references/discovery.md) for
fields, limits, context selection, and the script fallback when the binary or
release launcher is unavailable. Existing cloud cluster/search scripts remain
available for later investigation phases.

## Prometheus summaries

Query a recent range and list the series with the largest changes:

```bash
sre prom query 'sum by (pod) (rate(http_requests_total[5m]))' --context production --top 10
sre prom query 'up' --url https://metrics.example.com/prometheus --range 1h --step 30s --format text
```

Defaults are a one-hour range, 30-second step, 20 listed series, and a
30-second deadline for the whole operation. Range and step must be positive
whole milliseconds; fractional seconds such as `250ms` are supported.
Each series includes redacted
labels, finite first/last/min/max/mean, a possible level change, and counts of
missing steps, NaNs, and positive/negative infinities. Omitted series are
counted. Empty finite sets have null statistics. Raw samples are not stored.

Without `--url`, the command pins the selected Kubernetes context once and
uses discovery to select one Prometheus service, then reads its authenticated
API service proxy through a short-lived `kubectl proxy` on a private Unix
socket. The proxy accepts only GET for the selected query path and is stopped
and removed after the request, including on failure. It exposes no TCP listener
and preserves Prometheus HTTP error diagnostics. It prefers exposed port 9090, otherwise requiring one
exposed port. It does not start a port-forward or trust a localhost readiness
hint. Incomplete discovery, ambiguous services/ports, or proxy failures require
an explicit reachable `--url`. Explicit URLs preserve path prefixes and bypass
discovery. Prometheus errors and warnings fail the command.

The change point is a two-segment least-squares heuristic, with at least two
finite samples per side and a 20% reduction in squared error. It describes a
possible level shift, not statistical significance or causality. See
[Prometheus analysis](plugins/sre-agent/skills/sre-agent/references/prometheus-analysis.md)
for ranking, missing-data semantics, limits, and existing curl fallbacks.

## FinOps

The plugin also ships a `finops` skill for cost questions on AWS, GCP, and Kubernetes. It's separate
from incident response, and it never changes anything.

```text
/finops why did our AWS bill go up in October?
Reconcile our October GCP invoice with the BigQuery billing export.
Where can we save money on the search node pool, and is it safe?
Run the monthly cost report again, and check whether last month's savings landed.
Compare Kubecost's namespace costs with what we actually pay for the cluster.
```

It detects which billing data exists, asks once for a query budget, reconciles to the invoice, and saves
a report under `docs/finops/`. Every number in the report is read from a saved query result, calculated
by a script from such numbers, or marked as provided by a person, and a script proves this before the
report is written. That guards against mistakes. It can't prove a query asked the right question, or that
a cited cause is the true cause.

- It needs `python3` 3.9 or newer. Without it, the skill stops after discovery.
- GCP spend needs a billing export in BigQuery, and without one the report says so rather than estimating.
- Reports hold real spend figures, so add `docs/finops/` to your `.gitignore`. The skill warns if it isn't.
- Forecasting isn't included.

It hasn't been tried on real accounts yet. Its tests use stubbed CLIs and synthetic data, with its SQL run
in DuckDB, which proves query logic but not provider schemas. Treat the first live run as a trial.

Install it alone with `npx skills add glapsfun/sre-agent --skill finops`, or get it with the plugin.

## What it will never do

- Mutate anything — scale, restart, patch, apply, rollback — before you
  approve a specific remediation option. The approval gate is hard.
- Apply a change without a preceding dry-run and a stated rollback plan.
- Claim a fix is verified without live evidence for every expected-behavior
  criterion it defined in Phase 4.
- Run a mutating cloud or IaC command during evidence collection; Phase 3 is
  read-only and mechanically verified as such via a state snapshot/diff.
- Record secrets or credentials in the incident report — metadata and
  reasoning only.
- Buy commitments, resize anything, enable billing exports, or change IAM from a
  FinOps run. FinOps is advisory, and its queries run only under a budget you approve.

## Read-only enforcement

In a Claude Code plugin install, a hook (`hooks/readonly-guard.sh`) checks every command the seven
investigators run and allows only known read commands. Anything it doesn't recognize is blocked and
recorded as a gap. It also blocks reads of common credential files.

It stops mistakes and simple prompt injection, but it's not a sandbox. It can't see inside a script it
allows, a GET request can still carry data out, and it doesn't cover the main conversation, which is where
approved fixes run. It needs `python3`. Without it, the investigators are blocked and the skill runs their
playbooks inline.

The hard limit is a read-only identity, so investigate under a context that can't write:

```bash
kubectl create clusterrolebinding sre-agent-view --clusterrole=view --user=<read-only-user>
```

`view` leaves out Secrets and nodes, so those parts of the evidence become gaps. Use a read-only role in AWS
and GCP too. Each remediation option names its target context, so a fix can run under one that can write.

Other install paths get no hook. Codex documents that custom agent instructions don't create a read-only
sandbox and that subagents inherit the session's sandbox policy, and `npx skills` installs only the skill. On
those paths, the read-only identity is the only hard limit.

## Works best with

The orchestrator defers to sibling cloud-native skills for deep work when they
are available, and falls back to its own playbooks when they are not. Install
them from the
[cnative-skills marketplace](https://github.com/glapsfun/cnative-skills):
`kubernetes-operator`, `helm`, `fluxcd`, `argocd`, `karpenter`, `gh-guru`,
`glab-guru`, `gcloud`, `aws`, `bash-scripting`.

## Repository layout

```text
.claude-plugin/marketplace.json   # marketplace manifest (one plugin)
plugins/sre-agent/
├── .claude-plugin/plugin.json    # Claude Code plugin manifest
├── .codex-plugin/plugin.json     # Codex plugin manifest
├── commands/sre-agent.md         # /sre-agent slash command
├── commands/finops.md            # /finops slash command
├── hooks/                        # read-only guard for the 7 investigators
├── agents/                       # 7 read-only Claude subagents (generated)
├── skills/sre-agent/
│   ├── SKILL.md                  # the orchestrator: loop, phase gates, safety rules
│   ├── agents/codex/             # the same 7 subagents as Codex TOML (generated)
│   ├── evals/evals.json          # skill evals
│   ├── references/               # playbooks: discovery, PromQL, LogQL, ES DSL,
│   │   └── investigators/        #   TraceQL, mesh, RCA, remediation (Terraform,
│   │                             #   Pulumi, Crossplane), validation, k6, memory
│   └── scripts/                  # 11 read-only discovery/evidence helpers
└── skills/finops/
    ├── SKILL.md                  # the cost workflow, safety rules, ledger
    ├── agents/openai.yaml        # Codex interface
    ├── evals/evals.json          # skill evals
    ├── references/               # AWS and GCP billing, Kubernetes cost, assurance,
    │                             #   calculations, report, telemetry, troubleshooting
    ├── sql/                      # prepared billing queries
    └── scripts/                  # discovery, query runners, math, validation, report
scripts/                          # repo tooling: artifact generator + CI checks
tests/                            # script tests, SQL tests, synthetic fixtures
```

### Generated artifacts

The seven investigator playbooks in
`plugins/sre-agent/skills/sre-agent/references/investigators/` are the single
source of truth. The Claude subagents in `plugins/sre-agent/agents/` and the
Codex TOMLs in `plugins/sre-agent/skills/sre-agent/agents/codex/` are
**generated** from them — never hand-edit those. Edit a playbook, then run:

```bash
scripts/gen-sre-agent-artifacts.sh
```

CI enforces that they stay in sync.

## Development

```bash
scripts/validate.sh --fast   # structure, marketplace sync, generated-artifact
                             # drift, manifest versions, JSON, YAML, shell syntax
scripts/lint.sh              # shellcheck + shfmt
scripts/test.sh              # eval schema validation + script tests
scripts/security.sh          # gitleaks secret scan
```

## License

[MIT](LICENSE)
