# ADR 0003: Evidence boundaries and approval gates

Status: retrospective record of implemented behavior.

## Observed decision

The skill treats external text as untrusted data and requires explicit operator
approval, a named target, preview, rollback, and live validation before closing
an incident. Evidence services redact recognized secrets and wrap external data
in fresh random markers. Claude hooks restrict configured investigator calls;
Codex setup requires a verified parent seven-read allowlist with ledger writes
in a separate orchestrator-only session. A read-only identity is the external
access boundary.

Evidence: [skill rules](../../plugins/sre-agent/skills/sre-agent/SKILL.md),
[safety helpers](../../internal/safety/safety.go), [guard](../../internal/guard/guard.go),
[hook wrapper](../../plugins/sre-agent/hooks/readonly-guard.sh), and
[Codex setup](../../plugins/sre-agent/README.md#required-codex-investigator-permissions).

## Inferred rationale

Separating facts from instructions and evidence collection from remediation
appears intended to limit accidental mutation and instruction injection while
keeping the human responsible for operational change. This rationale is inferred;
no original threat-model decision or universal enforcement claim is asserted.

## Consequences and tradeoffs

The guard is not a sandbox and does not cover the main conversation or every
host. Snapshot comparisons inspect limited fingerprints and can be inconclusive.
Redaction cannot identify every possible secret. MCP annotations do not enforce
permissions, and a permission denial cannot be bypassed with another command.
Ledger validation checks recorded relationships and freshness, not the truth of
an observation or authenticity of human approval. Reports remain unresolved
without the required current validation history.

Evidence: [safety tests](../../internal/safety/safety_test.go),
[packaging checks](../../tests/sre-mcp-packaging.test.sh),
[ledger evaluator](../../internal/ledger/validation.go), and
[unresolved report tests](../../internal/report/report_test.go).
