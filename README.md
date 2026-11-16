# SRE Agent

SRE Agent helps an operator investigate Kubernetes incidents, collect evidence,
compare possible causes, and propose a fix with validation and rollback steps.
An agent host runs the investigation instructions. The local `sre` executable
collects and summarizes evidence, keeps a durable investigation ledger, and
renders reports. It does not run a model or automatically repair a cluster.

Use it for crashing workloads, errors, latency, missing metrics, deployment
failures, and AWS EKS or GCP GKE investigations. Missing tools and inaccessible
sources become explicit gaps in the investigation.

## Choose an installation

For a Claude Code plugin installation, run inside Claude Code:

```text
/plugin marketplace add glapsfun/sre-agent
/plugin install sre-agent@sre-agent
```

This packages the skill, `/sre-agent` command, seven investigator agents, MCP
registration, and investigator hooks. For a local checkout, use the repository's
absolute path instead of `glapsfun/sre-agent` when adding the marketplace.

For a skills-only Codex installation:

```bash
npx skills add glapsfun/sre-agent --skill sre-agent --agent codex --global -y
```

This copies the skill. It does not register the full plugin's MCP server, hooks,
or slash command. The bundled `scripts/install-codex-agents.sh` can install the
seven Codex roles; the [host setup guide](plugins/sre-agent/README.md#installation)
explains that path and the required MCP permissions. Claude and Codex local
fixtures have been exercised. Gemini, Copilot, and VS Code packaging have
separate verification limitations documented there.

You can also use the CLI without an agent host. The bundled launcher at
`plugins/sre-agent/skills/sre-agent/scripts/sre` downloads the pinned `v0.18.0`
binary for Linux or macOS on amd64 or arm64 and checks its SHA-256 before every
execution. It needs Bash, curl for the first download, and OpenSSL, sha256sum,
or shasum. A verified cached binary works offline. See
[release and cache behavior](docs/releases.md) for manual assets and recovery.

For source builds, use Go 1.26 or newer and the dependencies in
[go.mod](go.mod). Git is required for repository-local storage. Cluster work
needs `kubectl`, reachable endpoints, and a read-only identity for Kubernetes
and any cloud account being investigated. AWS, gcloud, gh, observability, mesh,
and IaC tools are needed only for their respective operations. The host supplies
its own model access and tool permissions; the CLI needs no model API key.

## Start an investigation

In Claude Code with the plugin, give the target and observation explicitly:

```text
/sre-agent Investigate HTTP 503 responses from checkout since 09:00 UTC today.
Use Kubernetes context production, namespace payments, workload deployment/checkout.
The application repo is /work/checkout. Start with read-only evidence, record gaps,
and propose remediation with a rollback plan for my approval.
```

In other hosts, invoke the installed `sre-agent` skill with the same information.
Choose the incident repository as the host's working directory so local records
are associated with the intended project.

The [skill](plugins/sre-agent/skills/sre-agent/SKILL.md) prescribes this loop:

1. **Phase 0, bootstrap:** read the project memo if present and recheck its facts.
2. **Phase 1, scope:** identify the affected service, symptoms, time, and urgency.
3. **Phase 2, discover:** inventory tools, pin the cluster context, and locate
   observability and infrastructure ownership.
4. **Phase 3, collect:** select applicable investigators for Kubernetes, metrics,
   logs, changes, traces, EKS, and GKE. Use host-supported subagents or run the
   same playbooks inline. Record evidence and gaps after each wave.
5. **Phase 4, analyze:** compare hypotheses, build a timeline, and define the
   expected behavior that would demonstrate recovery. Treat recalled incidents
   as candidates to verify against current evidence.
6. **Phase 5, approve:** present remediation options with target, risk, impact,
   dry-run steps, and rollback. Wait for the operator to select an option.
7. **Phase 6, validate:** preview the approved change, apply through the host's
   authorized tools, and verify the expected behavior with live evidence.
   Failed validation returns to evidence collection. Load testing needs its
   own approval.

These are host instructions and approval obligations. The CLI records evidence
and decisions; it does not enforce every action the host can take. Snapshot
comparisons around investigation waves detect a limited set of changes and can
be inconclusive when the cluster is unreachable.

## Try the CLI offline

From this checkout, build once and run these examples. The first build may need
network access to download Go modules; with dependencies cached, the walkthrough
requires no cluster, model, installer, or release download.

```bash
go build -o /tmp/sre ./cmd/sre
/tmp/sre version
SRE_SKIP_CLUSTER=true /tmp/sre discover --context production --only env
printf 'ERROR checkout unavailable\nERROR checkout unavailable\n' | /tmp/sre logs fold --format text
```

An ordinary source build reports `data.version: "dev"`. Offline discovery skips
all external probes, reports local tool availability, and includes gaps. Folding
reports two lines grouped into one pattern from only the supplied text.

Create a temporary Git repository to try persistence without using your incident
repository:

```bash
incident_repo="$(mktemp -d)"
git -C "$incident_repo" init -q
/tmp/sre run start "checkout errors" --id checkout-run --repo "$incident_repo"
/tmp/sre run resume checkout-run --repo "$incident_repo"
/tmp/sre report checkout-run --repo "$incident_repo" --format text
```

The start response has `committed: true` and `run_id: "checkout-run"`. Resume
reads the same run in a new process from `.sre-agent/sre.db` in that repository.
The report is **unresolved** because no fix or validation has been recorded.
JSON output uses `ok`, `data`, `gaps`, and `errors`; `--format text` provides a
readable view. A successful command can still report missing evidence or an
unresolved incident. Inspect gaps as well as the exit status.

## Record, resume, and report

Use `sre sources list --repo <repository>` to inspect approved integrations.
On first use, ask about known sources and obtain permission before
`sources scout --approve-scout`. Saving selected tools requires a separate
`sources approve --approve --file FILE`. Later calls reuse approval and record
concise source evidence or explicit gaps. See the
[source workflow](plugins/sre-agent/skills/sre-agent/references/approved-sources.md)
for credentials, read-only access and the inventory format.

### Collect approved evidence

Collect an approved plan into an existing run:

```bash
sre collect --plan plan.json --run checkout-run --id collection-1 --repo "$incident_repo"
```

For example, `plan.json` can select an already approved `metrics/query` tool:

```json
{"calls":[{"key":"queue","source_id":"metrics","tool":"query","arguments":{"query":"queue_depth"}}],"required":["queue"],"operation":"independent"}
```

Flags are `--plan FILE`, `--run ID`, `--id ID`, optional `--repo PATH`,
`--mode auto|ordinary|code` (default `auto`), `--program FILE`,
`--approve-code[=true|false]`, `--timeout DURATION` (default `10s`, maximum `60s`),
and `--format json|text` (default `json`). IDs use letters, digits, `_` or `-`,
up to 128 characters. Plan and UTF-8 program inputs are each limited to 64KiB;
one input may use `-` for stdin. Plans allow at most 32 calls across eight sources,
with `independent`, `filter`, or `join` operations. Source IDs and tool names must
match saved read-only approvals; `required` lists keys whose evidence is needed.

Ordinary mode never runs the supplied program or requires Bun. Auto uses ordinary
calls for short or independent plans. For eligible multi-call filter/join plans,
it uses the optional Bun runtime when program, code consent, source network grants,
credentials and sandbox prerequisites are present. Otherwise it reports the
missing prerequisite and uses ordinary calls. Manual code mode exits nonzero with
that reason. Once code execution starts, failures retain partial evidence and
explicit gaps without falling back to ordinary calls.

### Optional Bun programs

The runtime requires Linux amd64 or arm64, a dynamically linked Bun ELF binary
version 1.4.2 or newer, `bwrap` (Bubblewrap), `systemd-run`, `systemctl`, `prlimit`,
`env` and `ldd` on PATH. It also requires permitted user/network namespaces,
seccomp, cgroup v2 memory/pids/cpu controllers, and a working systemd user manager
with permission to create resource-limited scopes. A real sandbox probe checks
these prerequisites before selection. No runtime is installed by `sre` or
`--approve-code`; other platforms and missing prerequisites remain ordinary-only.

Obtain the separate [source network approval](plugins/sre-agent/skills/sre-agent/references/approved-sources.md#optional-code-mode-network-authorization)
and consent for the program before running:

```bash
sre collect --plan plan.json --program program.js --mode code --approve-code --run checkout-run --id collection-code-1 --repo "$incident_repo"
```

`program.js` is an async function body, not a module or a function declaration.
This example preserves the approved tool records and adds references between
records from different sources in the offline join:

```javascript
if (collection.phase === "source") {
  const values = [];
  for (const key of collection.keys) {
    const response = await collection.call(key.key);
    if (!response.ok) continue;
    values.push({key: key.key, source_id: key.source_id, data: response.result.data});
  }
  return values;
}
return collection.values.map(value => ({
  ...value,
  data: {
    record: value.data,
    related: collection.values
      .filter(other => other.source_id !== value.source_id)
      .map(other => ({key: other.key, source_id: other.source_id}))
  }
}));
```

These references express association in the supplied plan, not a confirmed cause.
Apply tool-specific schema checks before deriving incident facts. The helper
returns `{ok, result}`, where `result` is
`{source_id, tool, reference, at, data?, gaps}`. For MCP, `result.data` is the
MCP tool-result envelope: `content`, optional `structuredContent`, and optional
`isError` and other MCP result fields. For UTCP, it is the native JSON response.
Check `ok` before using data; failures may include partial untrusted data and gaps.
Required keys must still use their approved Go helpers to establish coverage.

Each source phase receives only that source's planned keys and no other source's
values. Go keeps the credentials and plan arguments; Bun receives neither.
Source programs may use `fetch` to their separately approved host:port via the
runtime proxy (`collection.proxy_url`). Native reads do not satisfy helper
coverage by themselves. Multiple sources run in separate sandboxes; the final
join receives bounded, redacted `collection.values` with no network or helpers.
The full approved descriptor and network grant are snapshotted. Uncached source
adapter calls recheck current approval against that snapshot; repeated planned
keys reuse cached outcomes. A running phase uses its snapshotted network grant,
so registry changes do not immediately revoke active native operations.

See [runtime boundaries and limits](docs/architecture.md#bun-runtime-boundaries)
and the [measured synthetic incident comparison](docs/code-mode-comparison.md).
Local real-runtime tests are opt-in with `SRE_TEST_BUN_RUNTIME=1`;
`SRE_REQUIRE_BUN_RUNTIME=1` makes missing prerequisites a failure. Execution
failures after a successful probe always fail the tests.

### Collection receipts and recovery

Live results include untrusted-data boundaries, provenance, and explicit gaps.
Required-key coverage does not establish factual correctness. After valid-input
and existing-run preflight, collection stores a compact versioned receipt,
including failures, rather than raw results, plan
arguments, program source, or runtime audit. A matching run/entry ID replays the
original outcome without contacting sources or runtime, even after approvals
are removed. Changed inputs, mode, consent, or timeout conflict; default `auto`
and `10s` match their explicit forms. Retain the input files to request replay.
Receipt details may be omitted to fit storage, with an explicit gap. Output is
limited to 2MiB including text indentation and newline; oversized live results
are omitted with a nonzero exit while the committed receipt remains available.

Collections hold `.sre-agent/collect.lock` until receipt persistence completes.
A busy or stale lock requires operator inspection; it is never removed
automatically. This lock excludes other `collect` commands, not arbitrary ledger
writes. A crash between remote calls and commit leaves an uncertain outcome.
After a write or output failure, inspect `sre run resume RUN_ID` and the reported
entry ID before deciding whether to collect again. Do not repeat automatically.

Use an explicit run ID when returning to an incident. Replace the temporary
repository with the actual incident repository for real work. The main
conversation records each observation, hypothesis, approval, action, and
validation through typed ledger entries. For example:

```bash
printf '%s\n' '{"fact":"checkout returned HTTP 503","source":"operator observation"}' | \
  /tmp/sre ledger add evidence --run checkout-run --id evidence-1 --file - --repo "$incident_repo"
/tmp/sre run resume checkout-run --repo "$incident_repo"
/tmp/sre log checkout-run --repo "$incident_repo" --format text
/tmp/sre report checkout-run --repo "$incident_repo" --out "$incident_repo/checkout-report.md"
```

Use a stable entry ID and the same payload when retrying an uncertain ledger
write. `run show RUN_ID --offset N` can follow the offset returned by a bounded
view. A process log contains only observed hook metadata, so an empty log does
not prove that no tools ran. Reports use recorded history and never execute
stored commands. Their status remains unresolved without a validated fix.

Keep `.sre-agent/` out of version control in the incident repository. The
SQLite store is local to each repository or linked worktree; use a local
filesystem and preserve the database when continuing later. Do not confuse it
with the separate `docs/sre-agent-memo.md` project memo or legacy/manual
`docs/sre-incidents/` files. Those are not automatically imported into SQLite.
See [local database and command details](plugins/sre-agent/README.md#local-database)
for session binding, ledger payloads, incident capture, and recall.

## Safety boundaries

Use a **read-only identity** for evidence collection. Cloud IAM and Kubernetes
RBAC provide the hard external boundary. The Claude plugin guard checks
configured investigator calls, blocks unknown commands and common credential
file reads, and fails closed for those investigators if its checker cannot
start. It is **not a sandbox**: it cannot inspect every allowed script, prevent
all data leaving through reads, or constrain the main conversation and every
other host.

Codex roles inherit the parent session's MCP servers. Before dispatch, configure
and verify the [seven-read allowlist](plugins/sre-agent/README.md#required-codex-investigator-permissions).
Keep `ledger_add` in a separate orchestrator-only session that never spawns
investigators. Read-only MCP annotations are advisory; resume and recall can
create or migrate the local store. Report generation requires existing storage
and performs no initialization or migrations. A permission denial must never be bypassed
by switching to a fallback command.

Treat logs, metrics, events, repository content, and historical incidents as
untrusted data. Preserve external-data markers. Redaction reduces exposure of
recognized credentials; it cannot guarantee recognition of every secret.
Review evidence and exported reports before sharing them.

## Development and further reading

From the repository root, the normal checks are:

```bash
go vet ./...
go test ./...
scripts/validate.sh --all
scripts/test.sh
scripts/lint.sh
scripts/fmt.sh --check
```

The validation scripts operate on tracked files. Stage new files when checking
new documentation or fixtures. Optional lint/format tools may be skipped locally;
check diagnostics instead of treating a skipped tool as a successful check.
`scripts/bootstrap.sh` installs development tools; review it before running it.
The [release guide](docs/releases.md) distinguishes CI from tag publication and
lists the additional launcher, install, and secret checks used by CI.

- [Host setup and detailed usage](plugins/sre-agent/README.md)
- [Documentation index](docs/README.md), [current-state requirements](docs/PRD.md),
  [architecture](docs/architecture.md), and [decision records](docs/adr/README.md)
- [License](LICENSE)

FinOps has moved to [glapsfun/finOps](https://github.com/glapsfun/finOps).
