# ADR 0001: Shared Go CLI and MCP

Status: retrospective record of implemented behavior.

## Observed decision

A single Go executable exposes command-line operations and a stdio MCP server.
Both adapters call the same discovery, logs, Prometheus, timeline, ledger,
incident, and report services. The host supplies the model and orchestration;
the executable supplies local operations. MCP exposes eight tools and classifies
only `ledger_add` as write.

Evidence: [entry point](../../cmd/sre/main.go), [CLI](../../internal/cli/cli.go),
[MCP adapter](../../internal/mcpserver/tools.go), and
[transport parity tests](../../internal/cli/mcp_test.go).

## Inferred rationale

Sharing domain services reduces the risk that a shell invocation and a typed
tool produce different evidence or validation decisions. Keeping the CLI usable
alone provides a direct diagnostic and recovery path when host registration is
unavailable. This rationale is inferred from the implementation, not a recovered
historical design discussion.

## Consequences and tradeoffs

A domain fix can affect both transports, so their contracts require parity
checks. Transport constraints still differ: the CLI supports stdin log input
and file report output; MCP requires explicit cluster context even for inline
logs or an explicit URL and caps the final encoded result at 8000 bytes.
Oversized reads route back to the CLI. Stdio avoids a network listener but
leaves process launch, authentication to external systems, and tool permission
filtering to the host and local environment.

Evidence: [schema tests](../../internal/mcpserver/server_test.go),
[result limiter](../../internal/mcpserver/result.go), and
[budget tests](../../internal/mcpserver/result_test.go).
