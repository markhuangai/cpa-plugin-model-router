# Usage Tracking

This document is the implementation contract for Model Router usage tracking. Keep it with the feature when rebasing or syncing changes from another remote.

## Scope

The feature adds a **Usage tracking** tab to the existing Model Router management page. **Configuration** remains the default tab, and the centered tab controls sit above the route table and **Add route** button.

Usage tracking covers both kinds of CPA traffic:

| Attribution | `router_model` | `provider_model` | Stored row |
| --- | --- | --- | --- |
| Routed | Client-visible Model Router alias | Physical CPA target | One row per attempted target, including failures before failover |
| Direct | Empty | Physical CPA model | One row per direct request |
| Unattributed official record | Empty | Model from CPA's usage record | One row when a safe correlation cannot be made |

Router and provider identities must remain separate throughout storage, filtering, aggregation, pricing, and display. A routed response can be rewritten to the alias for the client without changing the stored physical provider model.

The feature intentionally does not include:

- encryption or API-key recovery;
- API-key labels;
- batching or flush-interval controls;
- response compression controls;
- CNY conversion;
- CSV or image export;
- database backup or restore endpoints;
- a separate full-mode dashboard.

## Configuration and persistence

```yaml
plugins:
  configs:
    model-router:
      enabled: true
      data_path: "/var/lib/cliproxyapi/model-router-usage.db"
      retention_days: 365
      routes: []
```

| Field | Default | Validation |
| --- | --- | --- |
| `data_path` | CPA root plus `data/model-router-usage.db` | Converted to a clean absolute path; the parent directory is created with mode `0700` |
| `retention_days` | `365` | Integer from 1 through 3650 |

If `data_path` is absent, resolution checks:

1. the loaded shared-library path for an ancestor named `plugins`;
2. the CPA executable directory for a `plugins` directory;
3. the current working directory for a `plugins` directory;
4. `./data/model-router-usage.db` as the compatibility fallback.

Use an explicit absolute path inside a persistent volume when CPA runs in a container. Process restart persistence requires the next process to open the same file. Container replacement persistence requires that file's parent directory to be mounted outside the disposable container layer.

The database is dedicated to Model Router and uses bbolt with file mode `0600`. A successful record, price edit, or preference edit commits before its API call returns. There is no in-memory write batch to flush during shutdown.

The database contains these buckets:

```text
model-router-usage.db
├── meta
│   ├── schema_version
│   ├── next_sequence
│   ├── last_prune_day
│   ├── prices
│   └── preferences
├── requests
└── minutes
```

`requests` stores request/attempt details. `minutes` stores UTC minute aggregates keyed by attribution, router model, provider model, provider, source, service tier, and result. Overview queries currently read request records so filtering and price changes are reflected consistently; the aggregate bucket is retained for durable minute-level accounting and future query optimization.

Retention pruning runs when the database opens or is reconfigured and then at most once per UTC day during writes. Deleting expired bbolt keys makes pages reusable but does not guarantee a smaller file. Disk planning must account for peak retained request volume, including failed routed attempts.

**Reset usage** deletes and recreates only `requests` and `minutes`. It preserves model pricing and dashboard preferences in `meta`.

## Capture and deduplication

CPA's `usage_plugin` record is the preferred source because it contains normalized provider metadata. Some CPA versions enqueue `usage.handle` with a request context that is canceled as soon as the response finishes. Model Router therefore maintains a fallback path:

```text
request starts
    |
    +-- routed alias --> mark alias + physical attempt --> host.model.* response/stream
    |                                                |
    |                                                +--> parse fallback usage
    |
    +-- direct model --> request interceptor --> response/stream interceptor
                                                     |
                                                     +--> request.complete finalizes

official usage arrives in time --------> consume marker and store official record
fallback stores first -----------------> retain tombstone and suppress late official record
```

Correlation uses request time, provider model, and an in-memory keyed fingerprint of the client credential when available. The fingerprint secret is random for the process and is never persisted. Ambiguous markers with conflicting router identities are not guessed; the official record is stored as unattributed.

Fallback parsing supports the usage shapes used by:

- OpenAI Chat Completions;
- OpenAI Responses;
- Claude messages;
- Gemini `usageMetadata`;
- interactions payloads;
- JSON and Server-Sent Events, including usage split across chunks.

Streaming capture records time to first payload and merges cumulative usage by taking the greatest observed counter. Routed stream retry behavior is unchanged: an attempt can fail over only before any upstream payload is emitted.

## Stored data and privacy boundary

Each request row can contain:

- UTC request time and sequence;
- routed, direct, or unattributed classification;
- router alias and physical provider model as separate fields;
- provider, executor protocol, sanitized source, service tier, and reasoning effort;
- success/failure state and numeric status when available;
- latency, time to first token, generation duration, and output throughput;
- input, output, reasoning, cached, cache-read, cache-creation, and total tokens;
- a short masked API-key display value;
- an estimated USD cost resolved at query time.

The plugin does not persist:

- raw API keys or credential fingerprints;
- prompts or request bodies;
- model response bodies;
- failure bodies;
- response headers.

CPA can place an API key in `UsageRecord.Source`. For API-key authentication, source values equal to the API key, values with common credential prefixes, and high-entropy credential-like strings fall back to a provider/executor label. HTTP(S) sources retain only scheme, host, port, and path; URL user info, query strings, and fragments are removed.

The database is not encrypted. Protect its directory with filesystem permissions and volume access controls.

## Pricing

Prices are USD per one million tokens and are keyed by the physical provider model. Supported rates are:

- input;
- output;
- cache read;
- cache creation.

A model can also define ordered context thresholds, service-tier-specific schedules, and one of these input accounting modes:

| Mode | Meaning |
| --- | --- |
| `input_includes_cache` | Cache-read tokens are already part of input and are subtracted before applying the normal input rate |
| `input_excludes_cache` | Input and cache-read tokens are billed independently |
| empty | Provider-default calculation |

The price book has a monotonically increasing revision. Save and sync requests must name the revision they loaded; a stale revision returns `409` instead of overwriting another edit.

**Sync models.dev** fetches `https://models.dev/api.json` over HTTPS with a 15-second timeout and response-size limits. Matching uses provider priority, ignored suffixes, and explicit mappings. Manual rows are never overwritten by synchronized values.

Cost is calculated when querying. Changing a price updates estimates for retained history without rewriting request rows. Unmatched requests are counted as unpriced rather than silently treated as zero-cost.

## Dashboard behavior

The Usage tracking tab includes:

- preset and custom local-time ranges converted to UTC;
- minute, hour, and day granularity;
- router model, provider model, source, service tier, and result filters;
- token, cost, request, and top-model summaries;
- token, provider-share, cost, and efficiency charts;
- grouped usage and per-request tables with sorting, column controls, and pagination;
- model pricing and models.dev sync;
- an explicit typed confirmation for history reset.

The browser loads overview, groups, and request details in parallel. Existing metrics, charts, and rows stay mounted during a refresh. Each refresh owns an `AbortController` and generation number; only the newest active generation can render. Table scroll positions are restored after row replacement.

Polling runs every 15 seconds only when:

```text
Usage tracking is selected
AND the document is visible
AND no newer refresh has replaced the timer
```

Leaving the tab or hiding the document stops the timer and aborts the active request. Returning starts one fresh request set. Dashboard preferences are saved to bbolt after a short debounce.

The page follows CPAMC light, white, and dark themes, redraws canvas charts after theme changes, supports keyboard tab navigation, exposes visible focus states, and collapses controls and charts for mobile widths.

## Management API

All endpoints require CPA Management API authentication.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v0/management/plugins/model-router/usage/overview` | Summary, series, filter values, and storage status |
| `GET` | `/v0/management/plugins/model-router/usage/groups` | Sorted and paginated grouped usage |
| `GET` | `/v0/management/plugins/model-router/usage/requests` | Sorted and paginated request details |
| `GET` | `/v0/management/plugins/model-router/usage/prices` | Read the price book |
| `PUT` | `/v0/management/plugins/model-router/usage/prices` | Replace prices and sync settings at an expected revision |
| `POST` | `/v0/management/plugins/model-router/usage/prices/sync` | Synchronize observed models from models.dev |
| `GET` | `/v0/management/plugins/model-router/usage/preferences` | Read dashboard preferences |
| `PUT` | `/v0/management/plugins/model-router/usage/preferences` | Save dashboard preferences |
| `POST` | `/v0/management/plugins/model-router/usage/reset` | Delete usage after receiving `{"confirm":"reset"}` |

Overview, group, and request queries accept RFC3339 `from` and `to` plus optional `attribution`, `router_model`, `provider_model`, `source`, `service_tier`, and `result` filters. `router_model` always matches a literal route alias. Use `attribution=direct`, `attribution=unattributed`, or `attribution=routed` to filter by request origin; this keeps aliases named `direct` or `unattributed` distinct from the synthetic traffic classes. Group and request endpoints also accept `sort`, `order`, `offset`, and `limit`; the maximum page size is 500.

## Implementation map

```text
config.go                    storage configuration and validation
plugin_path*.go              default database path discovery
main.go                      plugin capabilities and usage callbacks
abi.go                       schema negotiation and lifecycle dispatch
attribution.go               correlation markers, ambiguity handling, tombstones
usage_capture.go             direct/routed fallback parsing and finalization
usage_types.go               persisted and API data contracts
usage_store.go               bbolt lifecycle, records, reset, prices, preferences
usage_query.go               filters, aggregation, sorting, pagination
usage_pricing.go             price validation and cost calculation
modelsdev.go                 catalog fetch, matching, and synchronization
usage_management.go          authenticated management endpoints
dashboard.html               Configuration and Usage tracking UI
integration_test.go          real CPA plugin, request, stream, and restart coverage
```

## Validation checklist

Before publishing changes to this feature, run:

```bash
make check
make build
CPA_SOURCE=../CLIProxyAPI \
  go test -tags=integration ./... -run TestModelRouterWithCLIProxyAPI -count=1
```

Then install the built library in a disposable CPA instance and verify:

1. Configuration is the default tab and keyboard tab navigation works.
2. Routed and direct streaming and non-streaming calls appear once.
3. A failed routed target appears separately from the successful retry.
4. Refresh keeps old values and scroll positions visible while requests are pending.
5. Prices and preferences remain after a process restart.
6. Usage, prices, and preferences remain after container recreation when `data_path` is mounted.
7. Light, white, and dark themes redraw charts without console errors.
8. Reset removes history but preserves prices and preferences.

## Origin

The dashboard information architecture, pricing workflow, persistence model, and operational lessons were adapted from [AITNR/cap-token-usage-tracker](https://github.com/AITNR/cap-token-usage-tracker) at commit `056175a7b3165365d0fffa36283430020aa9d900`. Model Router uses its own implementation and reduced scope. See [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md).
