# ADR 0002: Repository-local SQLite ledger

Status: retrospective record of implemented behavior.

## Observed decision

The Git root owns `.sre-agent/sre.db`, using a pure Go SQLite driver, private
filesystem permissions, WAL, a busy timeout, and transactional migrations.
Linked worktrees resolve to separate roots. Typed ledger entries append with
stable run/entry IDs and validated references; reduced state can be rebuilt
without the original conversation. Process-call metadata and captured incidents
share the database in separate tables.

Evidence: [store](../../internal/store/store.go), [schema](../../internal/store/migrations.go),
[ledger service](../../internal/ledger/service.go), [store tests](../../internal/store/store_test.go),
and [fresh-process resume test](../../internal/cli/ledger_test.go).

## Inferred rationale

Local transactional storage appears intended to retain investigation history
across process failures and conversation compaction without provisioning a
server. A pure Go driver permits CGO-disabled distribution. This is an inference
from dependencies, persistence tests, and the release build, not historical
proof of why SQLite was selected.

## Consequences and tradeoffs

Stable entry IDs make uncertain append retries safe when the payload is
unchanged; conflicting reuse is rejected. Reads may initialize or migrate the
store, so semantic read-only status does not mean zero filesystem writes.
Concurrent processes coordinate through SQLite, but there is no multi-machine
service, automatic backup, or historical Markdown import. Operators must keep
the store private, out of version control, and on a local filesystem suitable
for WAL. A newer schema is refused by an older binary rather than downgraded.

Evidence: [retry/concurrency tests](../../internal/ledger/service_test.go),
[process log](../../internal/processlog/log.go), and
[incident capture](../../internal/incident/capture.go).
