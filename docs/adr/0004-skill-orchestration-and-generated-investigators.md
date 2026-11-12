# ADR 0004: Skill orchestration and generated investigators

Status: retrospective record of implemented behavior.

## Observed decision

The skill defines Phase 0 bootstrap and Phases 1 through 6 as host instructions.
Seven investigator playbooks are the source for generated Claude agents and
Codex TOML roles. Symptom-based triage selects waves from applicable roles;
unsupported subagent execution falls back to inline playbooks. CI regenerates
both formats into a temporary directory and checks for drift.

Evidence: [skill](../../plugins/sre-agent/skills/sre-agent/SKILL.md),
[triage](../../plugins/sre-agent/skills/sre-agent/references/triage.md),
[investigator sources](../../plugins/sre-agent/skills/sre-agent/references/investigators/),
[generator](../../scripts/gen-sre-agent-artifacts.sh), and
[artifact check](../../scripts/checks/sre-agent-artifacts.sh).

## Inferred rationale

One playbook source appears intended to keep operational guidance aligned across
hosts, while waves limit unnecessary investigation and allow escalation. The
inline path makes the playbooks useful without subagent support. These are
inferences from structure and instructions, not claims about the original
selection of host integrations.

## Consequences and tradeoffs

Generated copies must be refreshed when source playbooks change. Equivalent
instructions do not imply equivalent host permissions, hook delivery, or runtime
support. Skills-only installation does not install full plugin registration.
Codex inherited MCP access requires parent configuration; role prose cannot
isolate tools. Claude/Codex local fixtures provide specific compatibility
evidence, while Gemini and Copilot/VS Code retain documented runtime limits.
The workflow depends on the host following the instructions; it is not a Go
state machine enforcing every phase transition.

Evidence: [packaging tests](../../tests/sre-mcp-packaging.test.sh),
[safety instruction checks](../../tests/sre-safety-docs.test.sh), and
[host setup](../../plugins/sre-agent/README.md).
