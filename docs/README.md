# SRE Agent documentation

Start with the [operator README](../README.md) for installation, an incident
prompt, the approval loop, and an offline CLI walkthrough.

- [Host setup and detailed usage](../plugins/sre-agent/README.md): MCP permissions,
  launcher behavior, ledger, reports, and host verification limits.
- [Current-state PRD](PRD.md): implemented requirements and their source/test evidence.
- [Architecture](architecture.md): host, local services, evidence, and persistence.
- [Releases](releases.md): CI, version alignment, publication, and cache recovery.
- [Retrospective ADRs](adr/README.md): observed decisions and their tradeoffs.
- [Investigation skill](../plugins/sre-agent/skills/sre-agent/SKILL.md): authoritative
  phase instructions and links to the detailed operational playbooks.

These documents describe the implementation at source commit
`310a177a57cdb56d105f05b38f73a979e986c9f7`, with the subsequent documentation
and FinOps-removal changes. The launcher and plugin manifests remain at 0.17.0.
They are current-state documentation, not a roadmap or a statement of historical
product intent.
