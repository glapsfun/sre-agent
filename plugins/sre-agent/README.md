# sre-agent

Agentic SRE orchestrator for [Claude Code](https://docs.anthropic.com/en/docs/claude-code), Codex, Gemini CLI, Copilot CLI, and any agent that supports the [Agent Skills](https://agentskills.io) standard — an assistant that helps human SRE engineers investigate and resolve operational incidents.

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
3. **Collect evidence** — Kubernetes state, Prometheus metrics, Loki or Elasticsearch/OpenSearch logs, Tempo/Jaeger traces, service-mesh state, recent changes (git, CI/CD, Helm, Flux/Argo), and cloud-specific evidence on AWS EKS (IRSA/VPC CNI, node group/Fargate capacity, control-plane logs, ALB/CSI/add-ons) and GCP GKE (Workload Identity/VPC-native networking, node pool/Autopilot capacity, Cloud Logging control-plane logs, Ingress/CSI/add-ons) — via seven read-only investigator playbooks, triaged into a cheaper first wave and an escalation wave by symptom class, dispatched in parallel as subagents where the host supports them (otherwise executed inline sequentially), with each wave's dispatch mechanically verified read-only via a live state snapshot/diff.
4. **Analyze**: build a timeline, rank root-cause hypotheses, and define *expected behavior* — the measurable criteria a fix must satisfy (the "failing test").
5. **Propose** 2–4 remediation options (description, steps, risk, pros/cons, impact, rollback) and **wait for your approval** — a hard gate.
6. **Apply, validate, iterate**: dry-run → apply → verify every expected-behavior criterion with live evidence — optionally under representative k6 load, behind its own approval gate. Pass → final incident report. Fail → back to step 3 with everything learned retained.

## Installation

The full 6-phase investigation works on every target. The difference is only
*how* Phase 3 evidence collection runs: with subagents (parallel, faster) or
inline (sequential, same evidence and format).

| Target | Install | Phase 3 |
| :--- | :--- | :--- |
| **Claude Code** | `/plugin marketplace add glapsfun/sre-agent` (once), then `/plugin install sre-agent@sre-agent` | Parallel subagents + `/sre-agent` command |
| **Codex** | `npx skills add glapsfun/sre-agent --skill sre-agent --agent codex --global -y`, then run the skill's bundled `scripts/install-codex-agents.sh` (installs to `~/.codex/agents/`; with `--project`, run it from your project directory — the target resolves against your cwd) | Parallel subagents (TOML, bundled). Skill alone = full investigation, sequential |
| **Gemini CLI** | `npx skills add glapsfun/sre-agent --skill sre-agent --agent gemini-cli --global -y` | Full investigation, sequential |
| **Copilot CLI** | `npx skills add glapsfun/sre-agent --skill sre-agent --agent copilot --global -y` (also picks up `.claude/skills/` installs) | Full investigation, sequential |

For Claude Code always use `/plugin install` — it additionally registers the
`/sre-agent` command and the seven subagents. After updating the skill on
Codex, re-run `install-codex-agents.sh` so the subagents stay in sync with
the playbooks.

## MCP tools

The source-built `sre mcp` command serves stdio only. It exposes exactly
`discover`, `logs_fold`, `prom_query`, `timeline`, `recall`, `run_resume`,
`ledger_add`, and `report`. It has no HTTP listener, resources, or authentication
layer. stdout carries only MCP messages; diagnostics go to stderr.
The launcher still pins `v0.17.0`, whose publication is tracked separately in
issue #5. Local source-build tests do not establish that a release is available.

Prefer available typed read tools for covered operations. Supply the exact
nonempty pinned cluster `context` to `discover`, `logs_fold`, `prom_query`, and
`timeline`, including inline log folding and automatic endpoint discovery.
Preserve the random external-data boundaries in returned text. `run_resume`
reads an explicit run without binding a session. `report` returns content and
has no output-file argument. Use the guarded Bash fallback for uncovered
operations or unavailable registration/server, and record the limitation.

The recommended Claude read permissions are individually named:

```text
mcp__plugin_sre-agent_sre__discover
mcp__plugin_sre-agent_sre__logs_fold
mcp__plugin_sre-agent_sre__prom_query
mcp__plugin_sre-agent_sre__timeline
mcp__plugin_sre-agent_sre__recall
mcp__plugin_sre-agent_sre__run_resume
mcp__plugin_sre-agent_sre__report
```

Investigators receive only the subset relevant to their role: discovery for
all seven; logs and timeline for Kubernetes/EKS/GKE; logs for the logs
investigator; Prometheus queries for metrics; timeline for changes. Trace
queries retain their guarded Bash recipes. Recall, resume, and report are
available to the orchestrator. `ledger_add` is the sole write tool and requires
a separate orchestrator permission decision. Never grant it to investigators.
A server-wide wildcard such as `mcp__plugin_sre-agent_sre__*` includes ledger
writes and is not the recommended read-only setup. `readOnlyHint` annotations
describe tools; they do not enforce permissions or replace the guard.

Claude loads plugin-root [`.mcp.json`](.mcp.json), whose command is
`${CLAUDE_PLUGIN_ROOT}/skills/sre-agent/scripts/sre` with the separate argument
`mcp`. Codex uses the portable [`plugin.json`](plugin.json) and
[`mcp.json`](mcp.json): `bash` receives
`${PLUGIN_ROOT}/skills/sre-agent/scripts/sre` and `mcp` as separate arguments.
That portable layout started and called `discover` in an installed Codex
0.167.1 fixture. The compatibility layout was recognized too, but needed an
absolute launcher path because its root placeholders stayed literal in that
runtime. These fixtures used a locally built binary, not a published release.

Gemini's [`gemini-extension.json`](gemini-extension.json) declares server `sre`
with `${extensionPath}/skills/sre-agent/scripts/sre` and the separate `mcp`
argument, following the [extension reference](https://geminicli.com/docs/extensions/reference/).
Gemini runtime expansion has not been tested locally because Gemini is absent.
Keep the placeholders literal in manifest files; the named host must expand
them. Parsing JSON alone does not verify startup or tool execution.

For manual registration, first locate the installed skill's executable
`scripts/sre`, or use the absolute path to a verified source-built `sre`.
The following commands are setup examples; register only after reviewing the
host's permissions. Codex's checked `mcp add --help` accepts:

```bash
codex mcp add sre -- /absolute/path/to/skills/sre-agent/scripts/sre mcp
```

Alternatively, add the same command and separate argument to the host's
Codex configuration:

```toml
[mcp_servers.sre]
command = "/absolute/path/to/skills/sre-agent/scripts/sre"
args = ["mcp"]
```

For [Copilot CLI](https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers),
use `/mcp add`, name the server `sre`, and enter the absolute launcher path
followed by `mcp` in Command. Select only the seven read tools above (using the
host's displayed tool names), with ledger writes decided separately.
For [VS Code](https://code.visualstudio.com/docs/agents/reference/mcp-configuration),
open Chat: Open Customizations, select MCP Servers, and add a stdio server
named `sre` with that executable and argument. Copilot's native configuration
uses `mcpServers`; older editor-managed `.vscode/mcp.json` uses `servers`.
Neither client was runtime-tested for this packaging change.

`npx skills` installs the skill without registering these plugin manifests.
Use manual registration or keep the CLI and bundled scripts. When MCP is
unavailable, locate the launcher and use `sre discover --context <ctx>`,
`sre logs fold --context <ctx> --namespace <ns> --pod <pod>`,
`sre prom query '<PromQL>' --context <ctx>`, or
`sre timeline --context <ctx> --namespace <ns>` with the operation's remaining
arguments. CLI stdin folding uses `sre logs fold < app.log` without cluster
flags. Local reads use `sre recall`, `sre run resume <run-id>`, and
`sre report <run-id>`. If the launcher or command is unavailable too, retain
the existing discovery/evidence scripts and report the gap. Investigator Bash
fallback remains subject to the read-only guard where installed and the
read-only identity on every host; a denied call is not a reason to bypass it.

## CLI launcher

Run `skills/sre-agent/scripts/sre version` from this plugin directory. The
launcher downloads the pinned `v0.17.0` release on first use for Linux or macOS,
on amd64 or arm64. It requires Bash, curl, and a SHA-256 tool: OpenSSL,
sha256sum, or shasum.
It prefers OpenSSL when available and verifies all binary bytes on every launch.
The binary prints one JSON object with `ok`, `data`, `gaps`, and `errors`.
Use `--format text` for a readable text view. Source builds provide `version`,
`discover`, `logs fold`, `prom query`, `timeline`, `run`, `ledger`,
`incident add`, and `recall`; the
existing investigation scripts remain available.
The pinned release may not yet include the source build's newer commands.

`sre logs fold` reads stdin and returns redacted patterns, counts, first/last
timestamps, levels, and samples. For example:

```bash
sre logs fold --max-patterns 20 < app.log
sre logs fold --context prod --namespace payments --pod checkout-abc \
  --container app --previous --since 1h --format text
```

Errors sort first, then warnings, then other levels; rarer patterns sort first
within each group. The default limit is 20 patterns. `omitted_patterns` and
`omitted_lines` account for everything outside the limit. Samples and templates
carry fresh external-data markers. Pod collection streams a finite snapshot,
enables timestamps, and uses a 30-second deadline. It selects explicit context,
then `SRE_KUBE_CONTEXT`, then the current context without changing it.
See [log folding details](skills/sre-agent/references/logs-investigation.md#fold-repeated-lines)
for masking, timestamp handling, and limits.

`sre timeline` returns every matching retained Kubernetes event, ReplicaSet
revision, Flux/Argo revision, local git commit and currently firing Alertmanager
alert in one sorted list. Namespace is required; workload accepts an exact name
or `KIND/NAME`. For example:

```bash
sre timeline --context prod --namespace payments --workload deployment/checkout \
  --since 2h --repo ./checkout --alertmanager-url http://localhost:9093 \
  --timeout 30s --format json
```

The default window is the last two hours, captured once in UTC. Context follows
`--context`, `SRE_KUBE_CONTEXT`, then one current-context lookup. Setup and all
collectors share the timeout. Alertmanager uses `--alertmanager-url`, then
`SRE_ALERTMANAGER_URL`; supply an already reachable HTTP(S) endpoint. The command
does not start a port-forward or use discovery hints as endpoints. Missing repo
or endpoint produces a gap. A failed source sets `ok=false` and exits 1 while
retaining independent successes; absence and historical-coverage gaps alone
exit 0. An unresolved requested workload never falls back to namespace scope.

JSON keeps the complete typed list at `data.entries`, plus all seven source
summaries, undated revision context and structured gaps. One fresh external-data
boundary marks the report; text presents the same records and coverage. There
is no entry limit or pagination. Events and ReplicaSets cover retained objects,
not deleted history; aggregate event counts do not date each occurrence.
ReplicaSet creation does not prove rollout or revision application time. Safe
pod-template comparisons omit literal environment values, annotations, commands
and other excluded fields while preserving references such as Secret names and
keys. Git covers local HEAD ancestry by committer time, not deployment time.
GitOps ownership must be verifiable; ambiguous remote associations and unknown
timestamps remain gaps or undated context. Alertmanager covers current firing
alerts, including suppressed alerts, not resolved alert history. Cloud audits,
GitHub activity, Helm history and mesh evidence still use the existing helpers.

The cache lives under `CLAUDE_PLUGIN_DATA/sre-agent` when set, otherwise
`XDG_CACHE_HOME/sre-agent`, otherwise `HOME/.cache/sre-agent`. Each version and
platform has its own directory. Downloads use HTTPS, bounded timeouts, and a
release `SHA256SUMS` manifest. The launcher verifies the checksum before
publishing a cache entry and before every execution. Checksums detect changed
downloads and cached binaries; they trust the release manifest and local cache.

A valid cached release works offline. With no cache, a failed download exits
nonzero and points to the existing scripts, such as `sre-env-discovery.sh` and
`sre-evidence.sh`. This also applies until `v0.17.0` is published. A checksum
mismatch refuses execution; remove the affected version cache and retry online.
A concurrent download exits with a retry message. After an interrupted download,
remove its stale `.lock` directory only after confirming no launcher is running.

Release tags build and run the version command on all four native platforms
before publishing binaries and `SHA256SUMS`. Updating the launcher pin is a
separate source change when adopting a later release.

## Local database

The CLI storage package creates `.sre-agent/sre.db` at the repository root on
first use. Linked Git worktrees get their own database. Keep `.sre-agent/`
out of version control in repositories where you use the agent. The directory
and database are private to the current user.

The store uses a pure Go SQLite driver, WAL, and a five-second busy timeout for
concurrent local processes. Use a local filesystem; SQLite WAL does not support
sharing a database between machines over a network filesystem. Schema upgrades
run in a transaction when the database opens. A database from a newer binary
is refused. After all users close it, a deleted database is recreated empty on
next use.

The investigation ledger stores append-only typed updates and redacts secret
values before each committed write. Existing incident Markdown and project
memos stay in place.

```bash
sre run start "checkout errors" --id checkout-run --repo ./checkout
sre ledger add evidence --run checkout-run --id evidence-1 --file evidence.json --repo ./checkout
sre run resume checkout-run --repo ./checkout
sre run show checkout-run --offset 1 --repo ./checkout --format text
```

For example, `evidence.json` contains one concise observation:

```json
{"fact":"checkout returned HTTP 503","source":"provided incident observation"}
```

The accepted kinds are `metadata`, `evidence`, `hypothesis`, `expected-behavior`,
`option`, `approval`, `action`, `validation`, and `safety-violation`. Use `--file -`
for stdin. Input is a single JSON object of at most 64 KiB with no unknown fields
or trailing JSON. Every add requires a stable `--id`; retry the same payload
with the same ID after uncertain completion. Run and entry IDs use 1-128 ASCII
letters, digits, underscores or hyphens. Start generates a random run ID when
`--id` is omitted. Retain the returned ID.

Ledger adds require `--run` or `SRE_RUN_ID`. An explicit resume ID takes precedence
over that variable; without either, resume reads the latest created run. Empty
storage reports `no runs recorded` and does not start a run. `--repo` resolves
the Git root, including when called from a subdirectory. `--session-id` on start
explicitly binds a known harness session; a metadata `session_id` update can
bind another session. No harness environment variable is assumed.

Resume and show include every ledger heading and page history within an
8192-byte response budget. JSON exposes `data.view.text` inside one fresh
external-data boundary, entry IDs, `through_seq`, `omitted_entries`,
`next_offset`, and `truncated_fields`. Follow the returned offset using show
or resume. Fields too large to display are shortened and named; full contents
remain in the local database. A collection that cannot fit produces an
identity-only diagnostic with the exact omitted-payload count and paths.
Entry offsets cannot recover oversized content within those payloads through
the bounded CLI; full records remain in the database and service API.
Validation always uses complete stored history
before presentation paging. A recorded pass checks consistency and freshness,
not the real-world truth of an observation.

A successful write returns `committed=true`. If display preparation fails after
the commit, success retains the run/entry identity and reports a gap directing
resume. An output failure exits nonzero, with stderr identifying the committed
run and entry; retry an uncertain add with the same ID. Record each update
immediately and resume after compaction or a new session. Ledger recording is
local bookkeeping in the main conversation; it grants no investigator write
permission and never executes stored remediation commands.

Generate a report from one explicit stored run with the source-built CLI:

```bash
sre report checkout-run --repo ./checkout --format text
sre report checkout-run --repo ./checkout --out ./checkout-report.md
```

The default JSON envelope carries `data.markdown`, `status`, `gaps`, `run_id`
and `through_seq`. Text prints the Markdown. Both place report content inside
fresh external-data boundaries; stored commands and links are data, never
instructions. Generation reads one consistent snapshot without changing ledger
or incident rows, executing recorded commands or fetching references. It does
not select the latest run or inherit `SRE_RUN_ID`.

Successful generation can return `status: "unresolved"`. Resolution uses the
shared ledger validation evaluator, including current criteria, action approval
and any recorded observation windows. Missing facts read `not recorded`;
missing-data and omission gaps remain visible. Multiline evidence facts and
action results retain only their first nonempty line, with omissions noted.

`--out` saves exactly the deterministic, sanitized Markdown without transport
markers and returns path, status, provenance, gaps and `written: true` metadata.
Relative output paths resolve from the current directory. The parent directory
must exist. A private temporary file in that directory is written, synced and
closed before atomic replacement, so ordinary failures preserve the old target
and remove the temporary file. Generation or file-write errors exit nonzero.
If only stdout fails after replacement, stderr states that the file was written
and the command exits nonzero. Report generation does not capture an incident
or modify existing manual incident files. See the
[reporting playbook](skills/sre-agent/references/validation-and-reporting.md)
for the hand-written fallback when this CLI is unavailable.

Validated incidents are captured from an explicit ledger run in the same local
store. Capture requires an applied remediation with every current expected
criterion passing, including any declared observation windows.

```bash
sre incident add --run checkout-run --service checkout --symptom errors --fingerprint "checkout HTTP 503" --repo ./checkout
sre recall --service checkout --symptom errors --fingerprint "checkout HTTP 503" --top 5 --repo ./checkout
sre recall --service checkout --symptom errors --fingerprint "checkout HTTP 503" --repo ./checkout --format text
```

`incident add` requires explicit `--run`; it never uses `SRE_RUN_ID` or the
latest run. Service and fingerprint must be nonempty. Canonical symptom classes
are `crashloop`, `latency`, `errors`, `metrics`, `oom`, `networking`, and
`availability`. Map descriptive symptoms to these before calling. `--top`
defaults to 5 and must be positive. JSON `data.matches` retains typed incident
records, numeric `score` and `similarity`, and `reasons`; `total` counts all
ranked candidates before limiting. Empty history returns `matches: []`,
`total: 0`, and `message: "no incident history"`. JSON and text mark historical
content as untrusted data with one fresh `begin`/`finish` boundary, including
nested text and map keys. Scores are local ranking heuristics, not confidence
estimates or proof of a shared cause.

Recall reads historical captures without changing ledger or incident rows and
never executes stored fixes. Confirm candidates against current evidence and
follow approval, dry-run and validation for every proposed remediation. Capture
returns `committed`, `run_id` and `through_seq`; presentation failure preserves
that receipt with a gap, and output failure reports the committed identity on
stderr. Retry with the same explicit run and signature: an unchanged snapshot
preserves the capture time; a later validated snapshot replaces the same row.
Invalid recapture leaves the previous validated incident intact.

Existing `docs/sre-incidents/INDEX.md` and incident files remain an explicit
legacy/manual source. The CLI does not import or export them, or stage, commit or
push files.
The project memo workflow remains separate. See
[`incident-memory.md`](skills/sre-agent/references/incident-memory.md) for the
Phase 4 and Phase 6 workflow.

## How to use

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

The agent then walks the loop above, keeping a visible **investigation
ledger** (environment, evidence with the command that produced each fact,
hypotheses with confidence, actions, validation results) updated at every
phase. When it reaches the approval gate it presents the options and stops —
pick one, ask for more evidence, or reject them all.

After resolution you get a final incident report (summary, impact, timeline,
root cause, actions, validation, rollback info, follow-ups) that you can save
to your repo.

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

## What's inside

| Component | Purpose |
| :--- | :--- |
| `skills/sre-agent/SKILL.md` | The orchestrator: loop, phase gates, safety rules, investigation ledger |
| `skills/sre-agent/references/investigators/` | The seven investigator playbooks (k8s, metrics, logs, changes, traces, eks, gke) — single source of truth; executed inline when subagents are unavailable |
| `agents/` — `sre-k8s-investigator`, `sre-metrics-analyst`, `sre-logs-investigator`, `sre-change-historian`, `sre-trace-analyst`, `sre-eks-investigator`, `sre-gke-investigator` | Read-only Claude Code subagents dispatched in parallel for evidence collection — generated from `references/investigators/` by `scripts/gen-sre-agent-artifacts.sh` |
| `skills/sre-agent/agents/codex/` | The same seven subagents in Codex TOML format (generated); installed by `install-codex-agents.sh` |
| `skills/sre-agent/references/` | Deep knowledge loaded on demand: discovery, PromQL (golden signals, kube-state, burn rates), LogQL and error taxonomy, Elasticsearch/OpenSearch query DSL, TraceQL/Jaeger tracing, deep Kubernetes evidence (nodes, NetworkPolicy, DNS, storage), service mesh (Istio/Linkerd), Grafana API discovery, root-cause analysis method, remediation templates, Terraform remediation (locating IaC-managed resources, safe plan reading, plan-classified apply/rollback), Pulumi remediation (locating IaC-managed resources, safe preview reading, preview-classified apply/rollback), Crossplane remediation (ownership detection, Ready/Synced condition and trace interpretation, status-check-verified apply/rollback), validation checklist and report format, k6 load validation, GitHub investigation via gh (repo discovery, change timeline, prior-incident and code search), sibling-skill deference (argocd/fluxcd/helm/bash-scripting/kubernetes-operator/karpenter/gh-guru/glab-guru), pinned official sources |
| `skills/sre-agent/scripts/` | Read-only helpers: `sre-env-discovery.sh` (tools, cluster, GitOps, cloud), `sre-obs-discovery.sh` (Prometheus/Alertmanager/Grafana/Loki/Mimir/Tempo/Jaeger/Elasticsearch endpoints, service-mesh and k6 detection), `sre-evidence.sh <ns> <workload>` (one-shot evidence pack), `sre-snapshot.sh` (Phase 3 mutation verification: snapshot/diff spec-hash fingerprints around each wave's dispatch), `sre-gh-discovery.sh` (read-only GitHub discovery/search via gh: repo/timeline/incidents/code subcommands), `terraform-plan-check.sh <tf-dir>` (Phase 5/6 Terraform plan classification: no-op/create/update/delete/replace counts, flags any destroy/replace), `pulumi-preview-check.sh <stack-dir>` (Phase 5/6 Pulumi preview classification: same/create/update/delete/replace counts, flags any destroy/replace), `crossplane-status-check.sh [TYPE[.VERSION][.GROUP][/NAME]]` (Phase 2-6 Crossplane evidence and verification: classifies Provider/Managed/Composite Resource Installed/Healthy/Ready/Synced conditions), `install-codex-agents.sh` (Codex subagent install) |
| `commands/sre-agent.md` | The `/sre-agent <problem>` entry point (Claude Code) |
| `commands/finops.md` | The `/finops <question>` entry point (Claude Code) |
| `hooks/` | The read-only guard for the seven investigator subagents (Claude Code) |
| `skills/finops/SKILL.md` | The FinOps workflow: scope, discovery, budget approval, evidence, assurance, math, and report |
| `skills/finops/references/` | AWS and GCP billing, Kubernetes cost, assurance, calculations, report fields, telemetry, troubleshooting, and which facts are confirmed |
| `skills/finops/sql/` | Prepared billing queries for CUR 2.0, legacy CUR, and the GCP export, exercised on fixtures in DuckDB |
| `skills/finops/scripts/` | `finops-discovery.sh` (no billable calls), `finops_aws.py`, `finops_gcp.py`, `finops_k8s.py`, and `finops_capture.py` (evidence), `finops_calc.py` (decimal math), `finops_record.py` and `finops_guard.py` (run record, read-only checks, budget), `finops_compare.py` (last report, past savings), `finops_validate.py` and `finops_report.py` (proof, then report) |

## Safety model

- **Read-only until approval** — investigation never mutates; only the main
  conversation applies changes, only after you select an option.
- **Dry-run first** — `kubectl diff`/`--dry-run=server`, `helm --dry-run`,
  `flux diff`, `terraform plan`, `pulumi preview` before every apply.
- **Secrets: metadata only** — names, ages, revisions; never values.
- **GitOps- and IaC-aware** — fixes to Flux/Argo-managed workloads, and to
  Terraform/Pulumi/CDK/eksctl-managed EKS infrastructure (node groups, IRSA
  roles, ALB config), are directed to the source repository/IaC, not
  hand-edited directly.
- **Gentlest effective action** — restart over delete, scale over replace;
  blast radius stated before anything destructive.
- **Load generation is mutation-class** — the optional k6 validation step has
  its own approval gate, states target environment/RPS/duration/blast radius
  up front, and never targets production unless you explicitly say so.

FinOps runs are advisory. They never buy commitments, resize anything, enable billing
exports, or change IAM, and their queries run only under a budget you approve.

## Read-only enforcement

In a Claude Code plugin install, a hook (`hooks/readonly-guard.sh`) checks every command the seven
investigators run and allows only known read commands. Anything it doesn't recognize is blocked and
recorded as a gap. It also blocks reads of common credential files.

It stops mistakes and simple prompt injection, but it's not a sandbox. It can't see inside a script it
allows, a GET request can still carry data out, and it doesn't cover the main conversation, which is where
approved fixes run. The packaged Go checker runs through the checksum-verified launcher. If it cannot
start, investigator calls fail closed; the main conversation and other agents remain outside the guard.

The pre-hook records each recognized investigator call in the repository's `.sre-agent/sre.db`.
Post success/failure hooks reconcile the same `(session_id, tool_use_id)` row. `guard=allowed` only
means the guard allowed the request; `outcome=unknown` does not establish execution or success.
Logging errors produce a nonblocking diagnostic and never change the guard's decision.

Bind the harness session when starting a run, or set `SRE_RUN_ID` to an existing run before launching
the harness. An explicit run ID takes precedence over session binding; hooks never select the latest run.

```bash
sre run start 'Investigate API latency' --id incident-1 --session-id SESSION_ID
sre log incident-1 --format text
sre log incident-1 --slowest 10 --by-investigator
sre log incident-1 --blocked --repo /path/to/repository
```

`log` defaults to JSON. Filters combine; investigator counts describe the selected rows after limiting.
Measured durations sort before unknown durations, with stable observation/session/call ordering for ties.
The timestamp records the first observation, not a tool start time. Absent duration and numeric exit
status remain null. The hook adapter does not infer process exit status from tool output or failure text.

Metadata contains sanitized command/path text, never complete inputs, responses, or failure messages.
Credential patterns, sensitive shell assignments, and user credential flags, including curl clusters,
are redacted before writing. Ambiguous credential arguments cause the
entire command text to be stored as `[REDACTED]`; simple values retain sanitized command context.
Identity fields that require redaction are rejected to avoid identity collisions. Do not place arbitrary
secret values in commands or identifiers.
The first observation pins the validated run and call metadata. Contradictory events or metadata produce
an absorbing `conflict` outcome and clear numeric metrics; a run-binding conflict leaves the original row
unchanged and emits a diagnostic. Repeated delivery cannot create another row for the same call.

The matchers cover every declared investigator tool: Bash, Read, Grep, Glob and WebFetch, plus
`mcp__plugin_sre-agent_sre__.*`. Glob records only its optional sanitized path; WebFetch records its
sanitized URL in the existing `path` field. Glob patterns, WebFetch prompts and responses are omitted.
Only the seven planned MCP read operations are allowed: `discover`, `logs_fold`, `prom_query`,
`timeline`, `recall`, `run_resume`, and `report`. `ledger_add` and unknown operations in this namespace are blocked for investigators.

Calls with no delivered hook cannot be logged. Permission denial can omit a post event, and schema
rejection can omit both events. Legacy pre events with all of `session_id`, `tool_use_id`, and `cwd`
absent remain guard-only; partially supplied or invalid metadata emits a logging diagnostic. Live MCP
and harness acceptance remain separate from the local hook-shaped integration tests.

The hard limit is a read-only identity, so investigate under a context that can't write:

```bash
kubectl create clusterrolebinding sre-agent-view --clusterrole=view --user=<read-only-user>
```

`view` leaves out Secrets and nodes, so those parts of the evidence become gaps. Use a read-only role in AWS
and GCP too. Each remediation option names its target context, so a fix can run under one that can write.

Other install paths get no hook. Codex documents that custom agent instructions don't create a read-only
sandbox and that subagents inherit the session's sandbox policy, and `npx skills` installs only the skill. On
those paths, the read-only identity is the only hard limit.

## Degraded environments

Missing tooling is the normal case, not an error. Every capability has a
fallback: no Prometheus → `kubectl top` + events; no Loki →
Elasticsearch/OpenSearch, else `kubectl logs`; no trace backend → latency
RCA proceeds metrics/logs-only; mesh present but no `istioctl`/`linkerd`
CLI → kubectl-only mesh evidence; no k6 → passive validation plus a script
you can run yourself; no cluster access at all → static analysis of repo
manifests with the limitation stated. Every gap is recorded in the ledger;
the agent never claims a fix is verified without evidence.

## Works best with

Install alongside the cloud-native plugins from the
[cnative-skills marketplace](https://github.com/glapsfun/cnative-skills) — the
orchestrator defers to them for deep work when they are available, and falls
back to its own playbooks when they are not:
[`kubernetes-operator`](https://github.com/glapsfun/cnative-skills/tree/main/plugins/kubernetes-operator), [`helm`](https://github.com/glapsfun/cnative-skills/tree/main/plugins/helm),
[`fluxcd`](https://github.com/glapsfun/cnative-skills/tree/main/plugins/fluxcd), [`argocd`](https://github.com/glapsfun/cnative-skills/tree/main/plugins/argocd), [`karpenter`](https://github.com/glapsfun/cnative-skills/tree/main/plugins/karpenter).
