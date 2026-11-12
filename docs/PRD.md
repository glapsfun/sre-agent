# Current-state product requirements

This PRD is reverse-engineered from the implemented SRE Agent at source commit
`310a177a57cdb56d105f05b38f73a979e986c9f7` and the accompanying operational
instructions. It records observable behavior and requirements enforced by code,
tests, or the skill. It does not establish original authorship, historical
intent, adoption metrics, service levels, or a future roadmap.

## Users and problem

The intended operator is an SRE, platform engineer, or developer investigating
an operational incident in Kubernetes, including EKS and GKE. They need to
assemble evidence from several systems, retain what was learned across
sessions, compare causes, and review a proposed change before it runs.
Maintainers also need reproducible agent packaging and a verifiable local CLI.
These roles are inferred from the [skill](../plugins/sre-agent/skills/sre-agent/SKILL.md)
and [CLI](../internal/cli/cli.go), not from customer research.

The product combines host-executed investigation playbooks with deterministic
local tooling. It helps answer what changed, what evidence supports a cause,
what recovery would look like, and whether recorded validation supports closing
the incident. It does not supply credentials, observability data, or a model.

## Workflow and use cases

An operator supplies symptoms, time, repository, context, namespace, and
workload as available. Phase 0 rechecks a saved project memo. Phase 1 scopes the
problem; Phase 2 inventories tools and pins the cluster. Phase 3 selects
applicable investigators in waves and records source-cited facts and gaps.
Phase 4 ranks hypotheses, consults historical candidates, and defines expected
behavior. Phase 5 waits for approval of a specific remediation. Phase 6 previews
and applies the approved change through the host, validates recovery, and
returns to investigation when criteria fail.

This supports live incident investigation, local analysis of supplied logs,
resuming an interrupted investigation, reporting an unresolved incident, and
recalling a previously validated resolution. Subagents are optional execution
mechanisms; inline playbooks remain available where the host lacks them.

## Implemented requirements and acceptance evidence

| Requirement | Implemented behavior | Source and verification evidence |
| :--- | :--- | :--- |
| Preserve operator control | Skill requires option approval, target pinning, dry-run, rollback, and separate load-test approval. The host executes changes. | [Skill](../plugins/sre-agent/skills/sre-agent/SKILL.md), [safety documentation checks](../tests/sre-safety-docs.test.sh) |
| Discover with partial information | Independent env, observability, AWS, GCP, and GitHub probes retain successes and distinguish missing, denied, timeout, and error. Offline mode skips subprocesses. | [Discovery](../internal/discover/discover.go), [probe implementations](../internal/discover/probes.go), [CLI tests](../internal/cli/discover_test.go) |
| Reduce log volume | Fold stdin or a finite pod snapshot into redacted patterns, counts, timestamps, levels, and samples with omission counts. | [Folder](../internal/logs/fold.go), [pod collection](../internal/logs/pod.go), [tests](../internal/logs/fold_test.go) |
| Summarize Prometheus evidence | Query a range at an explicit URL or one discovered service through the authenticated Kubernetes proxy; report statistics, nonfinite samples, and possible level changes. | [Query service](../internal/prom/query.go), [proxy](../internal/prom/proxy.go), [tests](../internal/prom/query_test.go) |
| Correlate retained changes | Merge Kubernetes events, ReplicaSets, Flux/Argo revisions, local Git history, and firing alerts; preserve source coverage and errors. | [Collector](../internal/timeline/collect.go), [types](../internal/timeline/types.go), [tests](../internal/timeline/collect_test.go) |
| Retain investigation state | Store typed, append-only ledger updates with stable IDs, validated references, redaction, transactions, and retry conflict detection. Resume without conversation memory. | [Ledger service](../internal/ledger/service.go), [types](../internal/ledger/types.go), [CLI persistence tests](../internal/cli/ledger_test.go) |
| Explain observed tool activity | Reconcile hook events by session and tool-use ID; retain sanitized metadata, unknown outcomes, and conflicts. | [Process log](../internal/processlog/log.go), [hook adapter](../internal/cli/hook.go), [tests](../internal/processlog/log_test.go) |
| Reuse only validated history | Capture from an explicit run with current approved action and passing criteria; recall ranks historical candidates without running fixes. | [Capture](../internal/incident/capture.go), [recall](../internal/incident/recall.go), [capture tests](../internal/incident/capture_test.go) |
| Report honestly | Generate a deterministic document from a stored snapshot. Missing fix or current validation keeps status unresolved; output-file failures are distinguished from presentation failures. | [Report](../internal/report/report.go), [file writer](../internal/report/write.go), [tests](../internal/report/report_test.go) |
| Share behavior across transports | CLI and eight stdio MCP tools call the same services. Cluster MCP tools require context; only ledger_add is classified write. | [CLI](../internal/cli/cli.go), [MCP tools](../internal/mcpserver/tools.go), [schema tests](../internal/mcpserver/server_test.go), [parity tests](../internal/cli/mcp_test.go) |
| Bound investigator access | Claude hooks guard named investigators. Codex requires a verified parent seven-read allowlist and a separate orchestrator write session. | [Guard](../internal/guard/guard.go), [hook manifest](../plugins/sre-agent/hooks/hooks.json), [host setup](../plugins/sre-agent/README.md#required-codex-investigator-permissions), [packaging tests](../tests/sre-mcp-packaging.test.sh) |
| Ship consistent roles and binaries | Generate both host role formats from seven playbooks; distribute pinned native binaries with per-launch checksum verification. | [Generator](../scripts/gen-sre-agent-artifacts.sh), [artifact check](../scripts/checks/sre-agent-artifacts.sh), [launcher tests](../tests/sre-launcher.test.sh), [release workflow](../.github/workflows/release.yml) |

## Scope and non-goals

The shipped CLI covers discovery, log folding, Prometheus summaries, timeline,
ledger and run operations, process logs, incident capture/recall, reporting,
hooks, and stdio MCP. Bash helpers and references cover additional cloud,
tracing, mesh, GitHub, IaC, snapshot, and validation operations. There is no
central control plane, web UI, scheduled monitoring service, automatic repair
controller, or HTTP MCP authentication layer in this implementation.

Approval is a requirement on the host workflow, not a universal runtime
interceptor. The ledger validates recorded relationships and observation
windows; it cannot prove that a human really approved an action or that an
observation is true. Report generation does not collect new evidence or execute
recorded commands. Recall is neither a root-cause oracle nor an auto-remediation
mechanism. Legacy/manual incident Markdown is a separate source, not an
import/export interface to SQLite.

## Safety and operational constraints

Use a read-only identity for cloud and Kubernetes evidence collection. The
investigator guard is not a sandbox and does not cover the main conversation,
inline execution, or every host. Snapshot comparisons cover selected state and
may be degraded. Permission denials remain gaps, never reasons to bypass the
same operation through another transport.

All external evidence and historical content remain untrusted data. Preserve
random data boundaries and avoid collecting credential values. Redaction covers
recognized patterns and known secrets; it cannot guarantee complete detection.
Keep the repository-local store private and out of version control. Local
semantic reads can create or migrate storage. Use a local filesystem for WAL;
there is no multi-machine database service or automatic backup facility here.

## Known limitations

- Host behavior varies. Claude/Codex local fixtures provide bounded evidence;
  Gemini was absent, and Copilot/VS Code were not runtime-tested for this packaging.
- A missing post-hook leaves unknown execution status. No delivered hook means
  no process-log record; this is not a complete transcript or audit of all activity.
- Discovery inventories and output views have bounds. MCP limits the final
  encoded result to 8000 bytes and directs oversized reads to the CLI. Ledger
  paging cannot retrieve every oversized individual field through its bounded view.
- Timelines use retained objects and local Git ancestry, not complete deployment
  or resolved-alert history. Correlation and Prometheus level changes do not
  establish causality or statistical significance.
- Source builds require Go and module availability. The launcher supports four
  Linux/macOS platforms and needs network access until a valid release is cached.
  Checksums depend on trust in the release manifest, without independent signing.
- Tag publication is not automatically recoverable by blind rerun after partial
  release creation or upload. See [release recovery](releases.md#partial-release-recovery).
