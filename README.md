# CPA Model Router

[![test](https://github.com/markhuangai/cpa-plugin-model-router/actions/workflows/test.yml/badge.svg)](https://github.com/markhuangai/cpa-plugin-model-router/actions/workflows/test.yml)
[![release](https://github.com/markhuangai/cpa-plugin-model-router/actions/workflows/release.yml/badge.svg)](https://github.com/markhuangai/cpa-plugin-model-router/actions/workflows/release.yml)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

CPA Model Router is a native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) model-router and executor plugin. It ports the configured model alias, priority routing, round-robin routing, cooldown, and failover behavior from `Z-M-Huang/CLIProxyAPI` into a standalone plugin for upstream CPA.

The plugin does not call providers directly. For each selected physical model it calls CPA's host model executor, so CPA keeps control of provider credentials, protocol translation, proxy policy, request logging, and usage capture.

## Features

- Expose logical model aliases through CPA's model registry.
- Route an alias to an ordered priority pool or a round-robin pool.
- Fail over on quota, rate-limit, auth, provider, server, and recognized transport failures.
- Cool failed targets in memory and skip them on later requests.
- Preserve a requested thinking suffix unless a target defines its own suffix.
- Rewrite non-streaming and streaming response model fields back to the requested alias.
- Retry a stream only before the first upstream payload is received.
- Preserve unchanged cooldown and round-robin state when CPA reconfigures the plugin.
- Configure ordered routes and target pools through a dedicated CPA management page.
- Track routed attempts and direct provider requests with separate router-model and provider-model identities.
- Persist request usage, minute aggregates, USD model prices, and dashboard preferences in a dedicated bbolt database.
- Review tokens, estimated cost, latency, TTFT, throughput, failures, and pricing coverage without replacing visible data during refreshes.

## Compatibility

The module is built against `github.com/router-for-me/CLIProxyAPI/v7` v7.2.123. It negotiates RPC schema v2 with older compatible hosts and schema v3 when offered; schema v3 avoids resending the full request body with every streaming response chunk. The CPA host must support native plugins, `model_router`, `executor`, `model_registrar`, request lifecycle and response interceptors, `usage_plugin`, and the `host.model.*` callback methods. The configuration and usage page also requires CPA's `management_api` capability and plugin resource menus.

Build the plugin for the same operating system and architecture as CPA. A Go `c-shared` library is not portable across OS or CPU targets.

## Configuration

```yaml
plugins:
  enabled: true
  dir: "/absolute/path/to/plugins"
  configs:
    model-router:
      enabled: true
      priority: 100
      data_path: "/CLIProxyAPI/plugins/model-router.db"
      retention_days: 365
      routes:
        - alias: auto
          strategy: priority
          cooldown_seconds: 60
          models:
            - gpt-5.4-mini
            - claude-sonnet-4-5
            - gemini-2.5-pro(8192)

        - alias: balanced
          strategy: round-robin
          cooldown_seconds: 30
          models:
            - provider-a/model
            - provider-b/model
```

`plugins.enabled` and `plugins.configs.model-router.enabled` must both be true. The library basename must be exactly `model-router` with the host extension: `.so`, `.dylib`, or `.dll`.

Only two usage-storage settings are supported:

| Field | Default | Meaning |
| --- | --- | --- |
| `data_path` | CPA `plugins/model-router.db` | Dedicated bbolt database path. Explicit relative paths resolve from the CPA process working directory. |
| `retention_days` | `365` | UTC days of request details and aggregates to retain, from 1 through 3650. |

When `data_path` is omitted, the plugin locates the CPA root from the loaded library, then the CPA executable, then the current working directory. It stores the database at `<CPA root>/plugins/model-router.db`; outside a detected CPA layout it uses `<working directory>/plugins/model-router.db`. The plugins directory must be writable and persistently mounted. Use an explicit absolute `data_path` when the plugin directory is read-only or stored on a disposable container layer.

Version 0.3.1 does not look for or migrate the former omitted-path default at `<CPA root>/data/model-router-usage.db`. To keep using that database, set its path explicitly before upgrading. The file is bbolt data despite the `.db` extension; it is not SQLite.

Every completed attempt is committed synchronously. Usage history, prices, and dashboard preferences therefore survive a normal CPA restart when the same database file remains mounted and writable. Retention deletes expired records and lets bbolt reuse their pages, but it does not guarantee that the file shrinks on disk. High-volume installations should monitor the database file and choose retention based on request rate and available storage.

### Configuration UI

The plugin registers a **Model Router** page in CPA's management frontend. Open the Plugins section and select **Model Router**. The centered **Configuration** and **Usage tracking** tabs appear above the route controls; Configuration is selected by default. When CPAMC has a persisted authenticated session, the page reuses its management key and loads configuration automatically. It also follows CPAMC's selected light, white, or dark theme and updates when that selection changes. The Configuration tab provides typed controls for route order, aliases, priority or round-robin strategy, cooldowns, and ordered target pools. Target dropdowns are populated with the model IDs currently returned by CPA's `/v1/models` endpoint.

Model discovery uses the management session to read CPA's configured client API keys, then keeps the first non-empty key in browser memory only while requesting `/v1/models`. The client key is not rendered or stored. If no client key is configured, the page attempts the model request without authorization for CPA installations without frontend authentication. Existing targets that are absent from the live catalog remain visible as disabled `<model> (unavailable)` choices until they are replaced, so loading the page never changes saved routes. New UI targets must be selected from the live catalog; custom or suffixed targets can still be managed through YAML or the Management API.

**Save changes** checks duplicate aliases, recursive routes, empty pools, duplicate targets, and cooldown values with the plugin's Go configuration parser before applying a shallow patch through CPA. The patch updates `enabled`, `priority`, and `routes` without replacing plugin-store metadata or unrelated config fields. The same page is available directly at:

```text
/v0/resource/plugins/model-router/config
```

The dashboard HTML is a public plugin resource so CPA can embed it in the frontend. Reading or changing configuration still requires the management key. The page reads CPAMC's persisted `cli-proxy-auth` value from same-origin browser storage; it does not change CPAMC's stored session. If **Remember password** is disabled, the persisted session has no key, so the page reveals a fallback key field. A fallback key is cached only in that tab's session storage and is removed when CPA rejects it. CPA's Management API must be enabled and reachable from the browser.

### Usage tracking

Usage tracking records one row for every physical routed attempt, including failed attempts before failover, and one row for direct provider requests. A routed row keeps the client-visible router alias in `router_model` and the physical CPA target in `provider_model`; direct rows have no router alias. This separation is used consistently in filters, summaries, grouped results, and request details.

The dashboard provides preset or custom time ranges, minute/hour/day trends, router/provider/source/tier/result filters, USD cost estimates, configurable columns, server-side sorting and pagination, and model pricing with optional models.dev synchronization. Chart tooltips work with pointer hover or keyboard focus, and provider-share segments and legend entries can toggle the provider-model filter. Provider, Source, Service tier, and Result start hidden in Usage breakdown; checkbox choices are saved in the database. Manual prices take precedence over synchronized catalog prices, and the pricing dialog closes after a successful save. Pricing units are USD per one million tokens; context tiers, service tiers, and cache accounting modes are supported.

Refreshes keep the previous dashboard visible while three fenced requests load in parallel. Starting a newer refresh aborts the older one, and only the newest generation may update the page. Automatic refresh runs every 15 seconds only while Usage tracking is selected and the document is visible. Reset deletes request history and aggregates but preserves prices and dashboard preferences.

The plugin prefers CPA's official usage record when it arrives in time. Because some CPA versions enqueue that record with a request context that is canceled at response completion, the plugin also captures usage from non-streaming responses, streaming chunks, and request-completion callbacks. A short-lived attribution marker suppresses a late official record after fallback storage, preventing double-counting.

See [docs/usage-tracking.md](docs/usage-tracking.md) for the data contract, operational guidance, API routes, and implementation map.

### Route fields

| Field | Required | Meaning |
| --- | --- | --- |
| `alias` | yes | Client-visible model name. Matching is case-insensitive. Thinking suffixes are not allowed on aliases. |
| `strategy` | no | `priority` by default, or `round-robin`. |
| `cooldown_seconds` | no | Seconds a failed target remains unavailable. Omitted or zero uses 60 seconds. |
| `models` | yes | Ordered physical model targets. Empty and duplicate targets are rejected. |

A route target cannot reference any configured route alias, including through a thinking suffix. This prevents recursive routes.

### Selection and failover

`priority` selects the first target that is not cooling down. `round-robin` advances the starting target after each selection and skips targets that are cooling down.

The plugin fails over for these numeric statuses:

- `401`, `402`, `403`, `408`, `429`
- `404`, except request-scoped persisted-item misses such as `store=false`
- all `5xx` statuses

`400` and `422` are treated as request errors and returned immediately. Cancellation and deadline errors never fail over. When the host callback loses a numeric status, the plugin fails over only for recognized quota, auth-unavailable, model/provider-unavailable, timeout, DNS, connection, broken-pipe, reset, or EOF messages. Ambiguous errors stop the route to avoid repeating a bad request against every provider.

A stream can move to the next target only when start-up fails before any payload bytes arrive. A failure after payload arrival cools the target but is returned to the client without splicing a second provider stream into the first.

### Thinking suffixes

If a client asks for `auto(high)` and the selected target is `gpt-5.4-mini`, CPA executes `gpt-5.4-mini(high)`. If the selected target is already `gemini-2.5-pro(8192)`, the target suffix wins.

Successful response fields such as `model`, `modelVersion`, `response.model`, and `message.model` are rewritten to the exact requested alias, including its suffix.

## Migrating From The Fork Configuration

Move the old top-level list under the plugin config and rename the canonical cooldown field from a hyphen to an underscore.

```yaml
# Before: Z-M-Huang/CLIProxyAPI fork
model-routes:
  - alias: auto
    strategy: priority
    cooldown-seconds: 60
    models:
      - gpt-5.4-mini
      - claude-sonnet-4-5
```

```yaml
# After: upstream CPA plus this plugin
plugins:
  enabled: true
  dir: "plugins"
  configs:
    model-router:
      enabled: true
      routes:
        - alias: auto
          strategy: priority
          cooldown_seconds: 60
          models:
            - gpt-5.4-mini
            - claude-sonnet-4-5
```

For staged migration, the plugin accepts `model-routes` instead of `routes` and `cooldown-seconds` instead of `cooldown_seconds` inside its config. Do not provide both forms of either field; registration fails rather than choosing one silently.

The fork-specific `/v0/management/model-routes` endpoints are not part of this plugin. The configuration page uses CPA's generic plugin endpoints, which remain available for automation:

```text
GET   /v0/management/plugins/model-router/config
PUT   /v0/management/plugins/model-router/config
PATCH /v0/management/plugins/model-router/config
PATCH /v0/management/plugins/model-router/enabled
```

## Build And Test

Go 1.26 and a working C compiler are required.

```bash
make check
make build
```

`make build` writes the host-platform library to `dist/model-router.<ext>`. The default suite covers strict config parsing, priority and round-robin state, reconfiguration, failure classification, header sanitization, model rewriting, non-stream failover, stream boundaries, usage parsing and attribution, persistence, pricing, management endpoints, the management page, and native RPC registration.

Run the opt-in black-box test against a local CPA source checkout:

```bash
CPA_SOURCE=../CLIProxyAPI \
  go test -tags=integration -run TestModelRouterWithCLIProxyAPI -count=1 -v
```

The black-box test builds CPA and the native plugin in a temporary directory, starts two logical providers on a local mock OpenAI-compatible server, loads the library through CPA, and verifies all of the following without real provider credentials:

- the logical alias appears in `/v1/models`;
- the Model Router menu and parser-backed validation endpoint are available;
- a `429` from the first target fails over to the second target;
- the client sees the requested alias in the response;
- the failed target remains on cooldown for the next request;
- routed and direct streaming and non-streaming usage remain distinct and are not duplicated;
- usage history, model prices, and dashboard preferences survive a CPA restart.

The test currently runs on Linux and macOS.

### Manual local installation

1. Run `make build` on the CPA host machine.
2. Copy `dist/model-router.so` on Linux, `dist/model-router.dylib` on macOS, or `dist/model-router.dll` on Windows into the configured `plugins.dir`. CPA also scans `plugins.dir/<goos>/<goarch>`.
3. Add `plugins.configs.model-router.enabled: true`. Add routes in YAML or through the Model Router management page. Every target model must already be routable by CPA.
4. Start CPA with `go run ./cmd/server --config /path/to/config.yaml` or your normal CPA binary.
5. Verify that the alias is listed:

```bash
curl -fsS http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer $CPA_API_KEY" \
  | jq '.data[] | select(.id == "auto")'
```

6. If the Management API is enabled, verify plugin registration:

```bash
curl -fsS http://127.0.0.1:8317/v0/management/plugins \
  -H "Authorization: Bearer $MANAGEMENT_PASSWORD" \
  | jq '.plugins[] | select(.id == "model-router")'
```

`registered`, `enabled`, and `effective_enabled` should all be true. A copied library is trusted in-process code; only load artifacts you built or verified.

## Current ABI Limits

- Routed token-count requests return HTTP `501` with code `model_route_count_tokens_unsupported`. The current host callback contract exposes model execution but not routed token counting. Returning an explicit error avoids reporting a false zero.
- Host execution errors do not expose the upstream `Retry-After` header to the plugin. Cooldowns therefore use `cooldown_seconds`, even when a provider asks for a longer delay.
- Nested host execution has no explicit requested-router metadata field. The plugin correlates official usage to a short-lived in-memory marker and falls back to response parsing when CPA does not deliver the official record.
- Status-less errors lose some structured failure information at the ABI boundary. The fallback classifier is conservative and may stop on an unfamiliar transient error until that error is added explicitly.
- Cooldown and round-robin state is process-local. It survives config reconfiguration when a route is unchanged, but not a CPA process restart.

## Security Notes

The plugin removes client `Authorization`, proxy authorization, cookies, host/content-length, and headers whose names contain API key, token, secret, or credential markers before calling `host.model.*`. CPA selects and applies the target provider credential. Other request headers and query parameters are preserved.

Usage storage never includes prompt text, request or response bodies, failure bodies, response headers, or raw API keys. The request table can store a short masked API-key display value. Source URLs are stripped of user info, query strings, and fragments, while values that resemble credentials fall back to a provider label.

The database is not encrypted by the plugin. Protect the configured path with filesystem permissions and volume access controls. Native plugins execute in the CPA process with CPA's permissions. Only load artifacts you built or verified, and do not put credentials in route model names.

## Publishing And Plugin Store Registration

The release workflow accepts tags such as `v0.3.1` and builds these CPA Plugin Store assets:

```text
model-router_0.3.1_linux_amd64.zip
model-router_0.3.1_linux_arm64.zip
model-router_0.3.1_darwin_amd64.zip
model-router_0.3.1_darwin_arm64.zip
model-router_0.3.1_windows_amd64.zip
checksums.txt
```

Each zip contains exactly one root-level library named `model-router.so`, `model-router.dylib`, or `model-router.dll`. The workflow embeds the tag version in plugin metadata and validates the archive layout before publishing.

To register the plugin publicly:

1. Push this repository to `https://github.com/markhuangai/cpa-plugin-model-router`.
2. Create and push a `v<major>.<minor>.<patch>` tag. Confirm the GitHub release contains all five zips plus `checksums.txt`.
3. Fork `router-for-me/CLIProxyAPI-Plugins-Store` and add this object to `registry.json`:

```json
{
  "id": "model-router",
  "name": "Model Router",
  "description": "Adds logical model aliases with priority or round-robin selection, cooldowns, and failover across CPA model targets.",
  "author": "markhuangai",
  "repository": "https://github.com/markhuangai/cpa-plugin-model-router",
  "homepage": "https://github.com/markhuangai/cpa-plugin-model-router",
  "license": "MIT",
  "tags": ["Router", "Model Router", "Fallback"]
}
```

4. Open a Plugin Store pull request that changes `registry.json`. Include the release tag and evidence that the platform archives and checksum file exist.

Do not add a `version` field unless the store maintainers request it. CPA discovers the latest version from the repository's newest published `v*` release.

After the registry PR is merged, install through the CPA management UI or `POST /v0/management/plugin-store/model-router/install`, then configure routes from the plugin's **Model Router** page or the generic config API.

## License

MIT. See [LICENSE](LICENSE). The usage dashboard and pricing workflow were adapted from AITNR's MIT-licensed CAP Token Usage Tracker; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
