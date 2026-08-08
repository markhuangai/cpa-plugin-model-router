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

## Compatibility

The module is built against `github.com/router-for-me/CLIProxyAPI/v7` v7.2.123. It advertises RPC schema v1 because it does not use the schema-v2 request-lifecycle additions. The CPA host must support native plugins, `model_router`, `executor`, `model_registrar`, and the `host.model.*` callback methods. The configuration page also requires CPA's `management_api` capability and plugin resource menus.

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

### Configuration UI

Version `0.2.0` registers a **Model Router** page in CPA's management frontend. Open the Plugins section, select **Model Router**, enter the CPA management key, and choose **Load configuration**. The page provides typed controls for route order, aliases, priority or round-robin strategy, cooldowns, and ordered target pools.

**Save changes** checks duplicate aliases, recursive routes, empty pools, duplicate targets, and cooldown values with the plugin's Go configuration parser before applying a shallow patch through CPA. The patch updates `enabled`, `priority`, and `routes` without replacing plugin-store metadata or unrelated config fields. The same page is available directly at:

```text
/v0/resource/plugins/model-router/config
```

The dashboard HTML is a public plugin resource so CPA can embed it in the frontend. Reading or changing configuration still requires the management key. The page keeps that key only in memory and does not use browser storage. CPA's Management API must be enabled and reachable from the browser.

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

`make build` writes the host-platform library to `dist/model-router.<ext>`. The default suite covers strict config parsing, priority and round-robin state, reconfiguration, failure classification, header sanitization, model rewriting, non-stream failover, stream boundaries, the management page, and native RPC registration.

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
- the failed target remains on cooldown for the next request.

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
- The host callback has no requested-model metadata field. CPA usage generated by nested execution is attributed to the selected physical target; the client response is still rewritten to the logical alias.
- Status-less errors lose some structured failure information at the ABI boundary. The fallback classifier is conservative and may stop on an unfamiliar transient error until that error is added explicitly.
- Cooldown and round-robin state is process-local. It survives config reconfiguration when a route is unchanged, but not a CPA process restart.

## Security Notes

The plugin removes client `Authorization`, proxy authorization, cookies, host/content-length, and headers whose names contain API key, token, secret, or credential markers before calling `host.model.*`. CPA selects and applies the target provider credential. Other request headers and query parameters are preserved.

Native plugins execute in the CPA process with CPA's permissions. Only load artifacts you built or verified, and do not put credentials in route model names.

## Publishing And Plugin Store Registration

The release workflow accepts tags such as `v0.2.0` and builds these CPA Plugin Store assets:

```text
model-router_0.2.0_linux_amd64.zip
model-router_0.2.0_linux_arm64.zip
model-router_0.2.0_darwin_amd64.zip
model-router_0.2.0_darwin_arm64.zip
model-router_0.2.0_windows_amd64.zip
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

MIT. See [LICENSE](LICENSE).
