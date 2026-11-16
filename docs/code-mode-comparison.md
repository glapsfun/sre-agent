# Bun and ordinary MCP incident comparison

Both modes retained the required synthetic incident facts, source attribution
and failure gaps in three local scenarios. Bun added explicit offline
associations, but used more supplied bytes, live-result bytes, fixture traffic
and elapsed time in every scenario. These small fixtures favor ordinary calls.
No model request was made; this run measures neither tokens nor model savings.

## Reproduce

Run from the repository root with the [Linux runtime prerequisites](../README.md#optional-bun-programs):

```bash
SRE_REQUIRE_BUN_RUNTIME=1 go test ./internal/cli -run 'TestCollectBunCLI|TestBunMCPIncidentComparison' -count=1 -v
```

The recorded run used Bun 1.4.2, Go 1.26.8, Linux 7.0.0-42-generic x86_64 on
2026-11-16. The reproduction command emits six machine-readable
`INCIDENT_MEASUREMENT` JSON rows. The full passing output was saved locally at
`.docs/reports/2026-11-16-bun-mcp-incident-comparison.log`; that scratch artifact
is not part of the repository. The measured rows are reproduced below.
Each required test fails if Bun or its sandbox prerequisites are absent. A
successful prerequisite probe is never used to excuse a later execution failure.
For all-package real-resource testing, use `GOFLAGS=-p=1` to serialize packages
sharing the systemd user manager.

## Fixtures and evidence oracle

All facts below are synthetic test data, not production metrics. Two local
provider servers each expose a real MCP SDK read tool and an equivalent native
GET route. The production Go source client performs discovery, tool verification,
credential handling and collection. MCP requests require the fixture credential;
tool arguments must explicitly request read-only access. Native routes expose
the same read data without credentials and reject every non-GET method. Each
source grant approves only its own server's host:port with an explicit fixture
read-only/no-anonymous-writes policy attestation.

Ordinary and code runs use identical serialized plans, fixtures and saved
approvals. Each code source phase calls its required Go helper, fetches the same
record from its own native endpoint, checks the two views agree by field, and
returns the native record. The offline join preserves both source records and
adds an association with its source/key and an explicit `association_only`
conclusion. The ordinary records support the same arithmetic and association;
ordinary mode does not execute or synthesize the JavaScript join. Thus evidence
equivalence means identical incident facts and supported conclusions, not
identical serialized envelopes.

| Scenario | Independent expected evidence | Outcome in both modes |
| --- | --- | --- |
| latency-dependency | checkout depends on inventory; checkout p95 900ms, baseline 100ms; inventory p95 750ms; slowdown 9 | Both records retained, required coverage true, zero gaps |
| deployment-errors | checkout rev-b versus rev-a; errors 2 before and 32 after, increase 30; deployed 2026-11-16T10:00:00Z, observed 2026-11-16T10:01:00Z | Both records retained, required coverage true, zero gaps |
| unavailable-source | Healthy checkout p95 600ms; second source fails | Healthy record retained, secondary key missing, failed-source and required-evidence gaps, no invented second record or derived conclusion |

`checkIncidentEvidence` uses explicit expected literals independent of fixture
and program construction. It checks factual contents, derived values, key/source/
tool/reference attribution, UTC timestamps, origins, activity outcomes, required
coverage, and exact failure reasons and impacts. A separate interval check binds
evidence, activity and audit timestamps to the measured invocation interval.
Core required-key validation establishes helper coverage; the independent oracle
adds factual completeness checks.

Controls remove each required evidence item, change facts while preserving
keys/provenance/coverage, corrupt provenance and activity, change derived values,
assert a false confirmed cause, and erase or falsely resolve the unavailable
source gap. Every mutation is rejected. Native counters and host proxy audit
prove two real native reads in each successful code scenario and one in the
unavailable scenario. An additional code run tries the other source's endpoint:
the proxy denies it before that fixture receives a request. Durable ledger checks
exclude raw programs, results, credentials and audit from compact receipts.

## Measured bytes and time

Plan bytes are the exact serialized JSON supplied to the CLI. Program bytes are
the exact UTF-8 input, zero for ordinary mode. Supplied input is their sum. Live
result bytes are `json.Marshal(collect.Result)` after CLI decoding, including
activity, audit and gaps, excluding the CLI presentation wrapper and durable
receipt. These are distinct dimensions, not additive estimates of model input.

| Scenario | Mode | Plan B | Program B | Supplied input B | Live result B | Wall microseconds |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| latency-dependency | ordinary | 303 | 0 | 303 | 1216 | 15340 |
| latency-dependency | code | 303 | 1476 | 1779 | 1675 | 429315 |
| deployment-errors | ordinary | 301 | 0 | 301 | 1324 | 15321 |
| deployment-errors | code | 301 | 1474 | 1775 | 1858 | 450801 |
| unavailable-source | ordinary | 303 | 0 | 303 | 1366 | 15450 |
| unavailable-source | code | 303 | 1476 | 1779 | 1544 | 437401 |

The same server instrumentation snapshots counters before and after each run.
Request bytes count the URI plus request body; response bytes count the bytes
written to the response body. Counts include MCP session initialization,
inventory, tool calls, teardown and native requests during collection. Initial
scouting, fixture setup and control runs are excluded. Each fixture request and
body is counted once, without adding proxy counters to network totals.

| Scenario | Mode | Requests | Request body + URI B | Response body B | Native reads | Gaps |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| latency-dependency | ordinary | 10 | 890 | 1434 | 0 | 0 |
| latency-dependency | code | 12 | 960 | 1582 | 2 | 0 |
| deployment-errors | ordinary | 10 | 888 | 1540 | 0 | 0 |
| deployment-errors | code | 12 | 956 | 1794 | 2 | 0 |
| unavailable-source | ordinary | 10 | 890 | 1332 | 0 | 2 |
| unavailable-source | code | 11 | 925 | 1384 | 1 | 2 |

Fixture byte counts exclude HTTP headers, transport framing, TLS and model
inference. Wall duration covers the public CLI invocation, including its own
runtime discovery/probe, process startup, collection, cleanup, receipt write and
output presentation. Test setup and the initial test-eligibility probe are outside
that interval. Timing is one local observation, not a benchmark claim; dynamic
ports, timestamps and scheduling can change exact byte/time results on rerun.

Every row records `input_tokens:null`, `output_tokens:null`, `cost_usd:null` and
`usage_reason:"No model request was made."`. No token estimates are inferred
from bytes.

## Effort and limits

The production integration adds the existing runtime constructor to the CLI's
default collection service. The comparison required two actual MCP/native
providers, approval setup, source-phase programs, an offline join, independent
fact/provenance/gap checks, mutation controls and network measurement. A real
provider also needs its tool schema, appropriate program logic, explicit network
approval and a provider-enforced read-only policy. This experiment does not
measure engineering hours or model authoring cost.

Ordinary calls remain preferable for short or independent work, these small
records, environments without the optional Linux sandbox, and providers without
an approved native read surface. Code may be useful when filtering or joining
larger approved results warrants program authoring and sandbox startup, but this
run does not establish that benefit. Required Go helper calls plus native reads
intentionally duplicate provider reads here to prove both coverage and actual
native evidence; code also emits audit and association data, increasing output.

The local HTTP fixtures establish these behaviors, not remote IAM correctness,
production performance or TLS method visibility. Provider read-only attestation
remains a policy prerequisite. Host audit observes proxy destinations and
denials, not direct attempts blocked inside namespaces or encrypted methods.
See [sandbox, transport, IPC and crash-artifact limits](architecture.md#bun-runtime-boundaries).
