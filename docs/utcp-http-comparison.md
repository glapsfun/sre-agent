# UTCP HTTP integration and comparison

The optional adapter calls an existing HTTP GET API using a UTCP 1.1 manual.
Ordinary MCP remains available. Both use the approved-source registry and the
same source evidence/gap receipts.

## Prometheus instant query example

Prometheus supports `GET /api/v1/query` with a `query` parameter and a JSON
response. See the [official API](https://prometheus.io/docs/prometheus/latest/querying/api/#instant-queries).
The following is an example manual to serve on the same origin as an existing
read-only Prometheus gateway; `metrics.example.test` is a placeholder.

```json
{
  "utcp_version": "1.1.0",
  "manual_version": "1.0.0",
  "tools": [
    {
      "name": "query",
      "description": "Read an instant Prometheus query",
      "inputs": {
        "type": "object",
        "properties": {"query": {"type": "string"}},
        "required": ["query"],
        "additionalProperties": false
      },
      "tool_call_template": {
        "call_template_type": "http",
        "url": "https://metrics.example.test/api/v1/query",
        "http_method": "GET",
        "auth": {
          "auth_type": "api_key",
          "api_key": "Bearer ${PROM_READ_TOKEN}",
          "var_name": "Authorization",
          "location": "header"
        }
      }
    }
  ]
}
```

The gateway must verify that its credential can only read the intended data.
Expose the manual as metadata through the same access boundary. This client does
not create a gateway, provision roles or turn unrestricted credentials into
read-only ones. GET alone does not establish permission safety.

An operator-provided known inventory identifies the manual and policy:

```json
{
  "sources": [
    {
      "id": "metrics",
      "name": "Prometheus metrics",
      "kind": "utcp-http",
      "endpoint": "https://metrics.example.test/utcp",
      "credential_env": "PROM_READ_TOKEN",
      "access_ref": "Verified read-only metrics gateway policy",
      "tools": []
    }
  ]
}
```

After scout consent, run `sre sources scout --approve-scout --known known.json`.
Copy selected descriptors and real digests into an approval file; after separate
approval run `sre sources approve --approve --file selected.json`. With an
existing run, `sre sources call metrics query --args query.json --run RUN --id
ENTRY` collects and records a concise receipt. `query.json` can contain
`{"query":"up"}`. No token value belongs in these files. Detailed boundaries are
in the [source workflow](../plugins/sre-agent/skills/sre-agent/references/approved-sources.md).

## Reproducible comparison

Run `go test ./internal/cli -run TestUTCPNativeComparisonAndCLIReceipts -count=1 -v`.
It exposes identical local Prometheus-shaped data through a native API/manual
and an MCP tool under the same fixture read-only credential. Two data scenarios
require every expected field plus provenance. An unavailable-source scenario
requires an explicit error and gap. Losing required evidence fails the test.

Measurements include current schema/manual verification and session cleanup.
Counters at the fixture server measure request URI/body bytes and written
response-body bytes, including error bodies the client may discard. Both modes
use their unmodified default clients; native calls use separate HTTP/1
connections to prevent transport retries. Headers, TLS and model inference are
excluded. Reported latency is one local test observation,
not a production benchmark. Model token usage and cost are explicitly unavailable
because this test makes no model requests; bytes are not labeled as tokens.

One local run on 2026-11-14 produced the following results. All six cases passed
required evidence or failure-gap checks. Timing varies between runs.

| Scenario | Transport | Requests | Request bytes | Response bytes | Result bytes | Latency (ms) |
| :--- | :--- | ---: | ---: | ---: | ---: | ---: |
| Queue depth | MCP | 5 | 419 | 722 | 161 | 4.028 |
| Queue depth | UTCP HTTP | 2 | 36 | 598 | 126 | 2.043 |
| Error count | MCP | 5 | 425 | 728 | 167 | 3.270 |
| Error count | UTCP HTTP | 2 | 42 | 604 | 132 | 2.059 |
| Unavailable | MCP | 5 | 419 | 634 | 73 | 3.042 |
| Unavailable | UTCP HTTP | 2 | 36 | 502 | 0 | 1.863 |

The unavailable UTCP case has no result data, but returns a separate explicit
gap and a failing status. Both transports preserve the gap in the comparison.
Model token usage and model cost are unavailable in every row.

## Integration effort and limits

| Concern | Native UTCP HTTP | MCP |
| :--- | :--- | :--- |
| Existing API | Add a manual describing the existing GET endpoint | Implement or operate an MCP tool that calls the API |
| Collection | Fetch pinned manual, then encoded native GET | Initialize, list/pin selected tool, invoke it, close session |
| Permissions | Provider identity plus selected template/schema | Provider identity plus selected tool/schema |
| Current scope | Same-origin GET and scalar arguments | Streamable HTTP MCP tools with read-only hints |
| Missing access | Explicit source gap and durable receipt | Explicit source gap and durable receipt |

The adapter follows a restricted subset of the [UTCP HTTP contract](https://www.utcp.io/protocols/http).
Neither protocol changes the provider's authorization model. Actual token cost
depends on tool exposure, returned data and host/model behavior; this comparison
does not establish token savings.
