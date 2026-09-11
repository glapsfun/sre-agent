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
├── agents/                       # 7 read-only Claude subagents (generated)
└── skills/sre-agent/
    ├── SKILL.md                  # the orchestrator: loop, phase gates, safety rules
    ├── agents/codex/             # the same 7 subagents as Codex TOML (generated)
    ├── evals/evals.json          # skill evals
    ├── references/               # playbooks: discovery, PromQL, LogQL, ES DSL,
    │   └── investigators/        #   TraceQL, mesh, RCA, remediation (Terraform,
    │                             #   Pulumi, Crossplane), validation, k6, memory
    └── scripts/                  # 11 read-only discovery/evidence helpers
scripts/                          # repo tooling: artifact generator + CI checks
tests/                            # script tests
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
