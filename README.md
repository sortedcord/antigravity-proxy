<div align="center">
<img src="docs/assets/antigravity_proxy_logo.webp" alt="antigravity proxy logo">
<br>
<b>Gemini ain't shit. Antigravity is.</b>
</div>

---

A bridge from the native Gemini API to Antigravity, using server-side Google OAuth. Built for Bifrost and more~

In other words, Antigravity Proxy allows you to use Gemini models in applications other than Antigravity, like pi, deepseek harness, hermes, etc.

> **Account risk:** This is an unofficial integration with internal Antigravity endpoints. Google may enforce its terms, including account suspension. Use an account you can afford to lose. This project is not endorsed by Google.

## Build and run

Requires Go 1.22 or later.

```sh
go build -o antigravity-proxy ./cmd/antigravity-proxy
./antigravity-proxy login
./antigravity-proxy serve
```

`login` prints Google's authorization URL and waits for a loopback OAuth callback (port `51121` by default, with fallback ports if busy). The resulting refresh token is stored in `~/.config/antigravity-proxy/config.json` with owner-only file permissions. You can use `go run ./cmd/antigravity-proxy login` and `go run ./cmd/antigravity-proxy serve` instead.

`login` obtains a complete OAuth client pair from environment overrides, then the saved config, then an installed `agy` found on `PATH` or at `$HOME/.local/bin/agy`. If no binary is installed, it follows the download-only flow from the [official CLI installer](https://antigravity.google/cli/install.sh): fetch the platform manifest and payload over HTTPS, verify the manifest's SHA-512 checksum, and read the native binary from private temporary storage. It never runs the installer or binary, installs `agy`, or changes shell configuration. Temporary downloads are removed after discovery.

The native binary is scanned for client IDs and secrets; when multiple identities exist, its Cloud Code OAuth initializer references determine the pair rather than string order or proximity. Unsupported or ambiguous binary layouts fail explicitly; update `agy` or supply both `ANTIGRAVITY_OAUTH_CLIENT_ID` and `ANTIGRAVITY_OAUTH_CLIENT_SECRET`. Automatic downloads follow the installer's Linux/macOS/Android amd64/arm64 manifests (including Linux musl); release availability remains upstream-controlled.

Successful login saves the exact client pair as `oauthClientId` / `oauthClientSecret` alongside the resulting token in the owner-only config file. Serving and token refresh use this saved pair and never discover/download a CLI, so a mounted login config also works in the scratch Docker image. Explicit environment credentials must be supplied as a complete pair and override the saved pair without mixing fields. Legacy configs with a refresh token but no client pair need another `login` or the original issuing-client pair; a new OAuth client cannot refresh a token issued to another client.

Login updates only saved OAuth token/client fields. It preserves unrelated settings already on disk and never persists runtime environment overrides or resolved defaults (including container-specific history paths). Config writes use private unique temporary files and sync before replacement. On Unix, existing config files must be owner-only (`chmod 600 ~/.config/antigravity-proxy/config.json`); unknown JSON keys, duplicate top-level keys (including spelling/case aliases), and trailing documents are rejected. Single case-insensitive field aliases remain accepted.

The service listens on `127.0.0.1:8080` by default:

```sh
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/models
```

`GET /models` returns Cloud Code's `fetchAvailableModels` JSON response unchanged. A configured Google credential and project are required; the project is discovered/provisioned when `projectId` is not configured.

Set an API key before exposing the listener beyond the local machine:

```sh
API_KEY='replace-with-a-long-random-value' HOST=0.0.0.0 ./antigravity-proxy serve
```

Startup rejects a non-loopback `HOST` unless `API_KEY` is set. When configured, the local API key protects `/models`, `/status/limit`, `/status/usage`, and every `/v1beta` endpoint. Send it in `x-api-key` or `Authorization: Bearer ...`; native Gemini and status endpoints also accept `x-goog-api-key` or the `key` query parameter. An unset key disables API-key authentication. `/health` remains unauthenticated and does not disclose credentials. This local key is never sent upstream: Cloud Code uses the service's Google OAuth credential.

## Docker

Build the image from the repository root:

```sh
docker build -t antigravity-proxy .
```

The multi-stage build produces a static Go executable. The minimal `scratch` runtime contains the executable and trusted CA certificates, runs as non-root UID/GID `10001:10001`, and has no shell, compiler, or `curl`. Its default command is `serve`; pass `login` explicitly to run OAuth login. Serving supports a read-only root filesystem **only with a separate writable quota-history mount**. It does not persist refreshed access tokens or write the OAuth config; it does persist quota observations. The image precreates `/home/app/.local/share/antigravity-proxy` owned by `10001:10001` but does not declare an automatic Docker volume. Without writable history storage, serving fails at startup.

Credentials belong at runtime, never in the image or build arguments. The `.dockerignore` allowlist excludes `.env` files, config files, Git history, assets, and local binaries from the build context. Do not add secrets to source files or otherwise include them in the build context.

### Serve with runtime environment variables

Export a long, random local `API_KEY`, your Google `ANTIGRAVITY_REFRESH_TOKEN`, and `ANTIGRAVITY_OAUTH_CLIENT_ID` / `ANTIGRAVITY_OAUTH_CLIENT_SECRET` from your secret source before running this example. The OAuth client must be the one that issued the refresh token; the local API key is a separate secret and is not sent to Google.

```sh
: "${API_KEY:?Set a long random local API key}"
: "${ANTIGRAVITY_REFRESH_TOKEN:?Set your Google refresh token}"
: "${ANTIGRAVITY_OAUTH_CLIENT_ID:?Set the matching Google OAuth client ID}"
: "${ANTIGRAVITY_OAUTH_CLIENT_SECRET:?Set the matching Google OAuth client secret}"
export API_KEY ANTIGRAVITY_REFRESH_TOKEN ANTIGRAVITY_OAUTH_CLIENT_ID ANTIGRAVITY_OAUTH_CLIENT_SECRET

docker run --rm --name antigravity-proxy \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --mount type=volume,src=antigravity-quota-history,dst=/home/app/.local/share/antigravity-proxy \
  -p 127.0.0.1:8081:8080 \
  -e API_KEY \
  -e ANTIGRAVITY_REFRESH_TOKEN \
  -e ANTIGRAVITY_OAUTH_CLIENT_ID \
  -e ANTIGRAVITY_OAUTH_CLIENT_SECRET \
  antigravity-proxy
```

Unlike the host executable, the image defaults to `HOST=0.0.0.0` and `PORT=8080` so Docker can reach the listener inside the container. `API_KEY` is therefore required: startup rejects this non-loopback listener without it, even if the published host port is loopback-only. The mapping above exposes container port `8080` at host `127.0.0.1:8081`; it does not change the container's `PORT`. Keep host publication loopback-only unless you intentionally configure secure remote access.

The named `antigravity-quota-history` volume stores `usage.jsonl` and survives `--rm`; reuse it to retain observations across restarts. This example uses the image's default UID/GID. The image sets `ANTIGRAVITY_QUOTA_HISTORY_PATH=/home/app/.local/share/antigravity-proxy/usage.jsonl`, overriding any host-specific `quotaHistoryPath` saved in the mounted JSON config. To use a custom container history path, override this environment variable at runtime and mount its parent directory writable; changing only the JSON field is not enough. The root filesystem and any OAuth config mount can remain read-only.

Check local health from the host with `curl http://127.0.0.1:8081/health`. For protected routes, send the same `API_KEY` in a supported authentication header. A directly supplied `ANTIGRAVITY_ACCESS_TOKEN` can replace the refresh-token flow, but it expires and is not refreshed automatically. Environment variables are visible to users with Docker access; keep that access restricted.

### Use an existing login config

Run `./antigravity-proxy login` on the host as described above; client discovery is automatic unless a complete environment or saved pair is configured. Then mount the resulting config directory read-only for serving instead of passing `ANTIGRAVITY_REFRESH_TOKEN`. The saved client pair travels with the refresh token, so OAuth client environment variables are not required in this example.

On native Linux with rootful Docker and no user-namespace remapping, run as your host UID/GID so the container can read the owner-only (`0600`) config file:

```sh
mkdir -p "$HOME/.local/share/antigravity-proxy"
docker run --rm --name antigravity-proxy \
  --user "$(id -u):$(id -g)" \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --mount type=bind,src="$HOME/.config/antigravity-proxy",dst=/home/app/.config/antigravity-proxy,readonly \
  --mount type=bind,src="$HOME/.local/share/antigravity-proxy",dst=/home/app/.local/share/antigravity-proxy \
  -p 127.0.0.1:8081:8080 \
  -e API_KEY \
  -e ANTIGRAVITY_QUOTA_HISTORY_PATH=/home/app/.local/share/antigravity-proxy/usage.jsonl \
  antigravity-proxy
```

The image keeps `HOME=/home/app` even when `--user` overrides its numeric identity. The writable history bind above is created by your host user so it matches the overridden UID/GID; do not reuse a volume owned by the image's `10001:10001` identity under a different UID. Rootless Docker and user-namespace remapping map host owners differently: use runtime token environment variables or a named volume owned by the appropriately mapped container user instead, with writable history storage owned by that same identity. Do not make credential files world-readable to work around ownership.

Container login is optional. The OAuth callback binds to container loopback (`127.0.0.1`), so publishing `-p 51121:51121` on a bridge network does **not** make it reachable from the host browser. On native Linux with rootful Docker and no user-namespace remapping, host networking shares the host loopback and permits this alternative. It uses automatic CLI download/discovery and a writable temporary filesystem (nothing is installed):

```sh
mkdir -p "$HOME/.config/antigravity-proxy"
docker run --rm --network host \
  --user "$(id -u):$(id -g)" \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=1g \
  --mount type=bind,src="$HOME/.config/antigravity-proxy",dst=/home/app/.config/antigravity-proxy \
  -e HOST=127.0.0.1 \
  antigravity-proxy login
```

The login mount is writable so the token can be saved; switch back to the read-only config mount and separate writable history mount for serving afterward. `login` does not poll quotas or write history, so it needs no history mount. Follow the authorization URL printed by `login` in your host browser. Do not assume this host-network callback flow is portable to Docker Desktop; host login is the recommended option.

### Connect Bifrost

Use Bifrost's built-in Gemini provider and the configuration in [Bifrost setup](#bifrost-setup). With the host-port mapping above, a Bifrost process on the host uses `http://127.0.0.1:8081/v1beta`. Set Bifrost's `ANTIGRAVITY_PROXY_API_KEY` to the same value as the container's `API_KEY` and retain the `env.ANTIGRAVITY_PROXY_API_KEY` provider key reference.

If Bifrost is also containerized, attach both containers to the same user-defined Docker network (create one with `docker network create antigravity` and add `--network antigravity` to the proxy run). Use `http://antigravity-proxy:8080/v1beta` as Bifrost's provider base URL, using the proxy's container name and internal port, not Bifrost's own `localhost` or the published host port. Host publication is optional for this container-to-container path. If your Bifrost deployment blocks private-network provider destinations, allow this trusted destination through its private-network access controls; consult the [Bifrost provider configuration](https://docs.getbifrost.ai/quickstart/gateway/provider-configuration) for your installed version. In either topology, `/v1beta` and the matching local API key are required.

## Endpoints

| Method and path | Behavior |
| --- | --- |
| `GET /health` | Local health and whether a Google credential is configured; unauthenticated |
| `GET /models` | Raw Cloud Code `fetchAvailableModels` JSON, unchanged |
| `GET /status/limit` | Latest persisted quota snapshot for Gemini and the shared Claude/GPT/other pool, with five-hour and weekly windows, polling metadata, and staleness |
| `GET /status/usage` | Filtered, paginated history of observed quota windows, not billable request/token usage |
| `GET /v1beta/models` | Native Gemini model list with `pageSize`, `pageToken`, and `nextPageToken` pagination |
| `GET /v1beta/models/{id}` | Native Gemini metadata for one catalog model |
| `POST /v1beta/models/{id}:generateContent` | Native Gemini generation request and response |
| `POST /v1beta/models/{id}:streamGenerateContent?alt=sse` | Native Gemini SSE generation; `alt=sse` is required |

The native catalog is derived dynamically from Cloud Code's external generative model IDs, including Gemini, Claude, and GPT models; internal/tab/lead-in metadata is excluded. There is no hardcoded model or capability list. Models advertise only `generateContent` and `streamGenerateContent`; token limits are included only when real catalog metadata supplies them, otherwise omitted. A listed model is not a promise that every generation option is supported: the upstream model validates capabilities.

### Native API examples

Use a local secret for both the service and clients. Start the service in one terminal (after OAuth login):

```sh
export ANTIGRAVITY_PROXY_API_KEY='replace-with-a-long-random-value'
API_KEY="$ANTIGRAVITY_PROXY_API_KEY" ./antigravity-proxy serve
```

Set the same `ANTIGRAVITY_PROXY_API_KEY` in the client terminal:

```sh
export ANTIGRAVITY_PROXY_API_KEY='replace-with-a-long-random-value'
export ANTIGRAVITY_PROXY_URL='http://127.0.0.1:8080'

curl "$ANTIGRAVITY_PROXY_URL/v1beta/models?pageSize=100" \
  -H "x-goog-api-key: $ANTIGRAVITY_PROXY_API_KEY"

# Use a model ID returned by the catalog; this is an example, not a fixed list.
export MODEL_ID='gemini-3-flash'
curl "$ANTIGRAVITY_PROXY_URL/v1beta/models/$MODEL_ID" \
  -H "x-goog-api-key: $ANTIGRAVITY_PROXY_API_KEY"

curl "$ANTIGRAVITY_PROXY_URL/v1beta/models/$MODEL_ID:generateContent" \
  -H "x-goog-api-key: $ANTIGRAVITY_PROXY_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"contents":[{"role":"user","parts":[{"text":"Explain why the sky is blue in one sentence."}]}]}'

curl --no-buffer "$ANTIGRAVITY_PROXY_URL/v1beta/models/$MODEL_ID:streamGenerateContent?alt=sse" \
  -H "x-goog-api-key: $ANTIGRAVITY_PROXY_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"contents":[{"role":"user","parts":[{"text":"Write a short greeting."}]}]}'
```

If a list response includes `nextPageToken`, pass its value back as `pageToken` (URL-encoded) to retrieve the next page. Native model IDs are unprefixed; do not send a Bifrost `gemini/` prefix to these endpoints.

### Bifrost setup

Configure Bifrost's built-in **Gemini** provider, not an OpenAI-compatible custom provider. To keep Bifrost's usual port separate, run this proxy on port `8081`:

```sh
export ANTIGRAVITY_PROXY_API_KEY='replace-with-a-long-random-value'
PORT=8081 API_KEY="$ANTIGRAVITY_PROXY_API_KEY" ./antigravity-proxy serve
```

Make that same environment variable available to the Bifrost process and add this provider to its `config.json`:

```json
{
  "providers": {
    "gemini": {
      "keys": [
        {
          "name": "antigravity-local",
          "value": "env.ANTIGRAVITY_PROXY_API_KEY",
          "models": ["*"],
          "weight": 1
        }
      ],
      "network_config": {
        "base_url": "http://127.0.0.1:8081/v1beta"
      }
    }
  }
}
```

**The base URL must include `/v1beta`.** Bifrost appends `/models` and the model action paths and sends `x-goog-api-key`. The key above is the proxy's local `API_KEY`, not a Google API key or OAuth token; Google OAuth credentials stay in the proxy. The origin must be reachable from Bifrost: loopback works only when both processes share the same host/network namespace. For separate containers or hosts, use a reachable address and the listener/auth configuration below.

Select `gemini/<catalog-id>` in Bifrost gateway requests, even for a Claude or GPT catalog ID routed through this Gemini transport. Bifrost handles its gateway text/chat/Responses formats; this proxy itself accepts only the native Gemini routes above. Current Bifrost model listing requests `pageSize=1000`; direct native clients can use pagination.

See Bifrost's primary [Provider Configuration](https://docs.getbifrost.ai/quickstart/gateway/provider-configuration) and [Provider Setup (`providers.md`)](https://docs.getbifrost.ai/deployment-guides/config-json/providers.md) documentation for provider keys, environment references, and network configuration.

### Supported behavior and limits

- Native generation preserves request and response fields, including `systemInstruction`, tools/function calls and responses, thought signatures, and inline image/video data. Text generation, video understanding, and native Gemini image generation/editing use the same generation routes, only when the selected upstream model supports the request. Forwarding tool fields does **not** establish support for Google Search, code execution, TTS, or other built-in Google APIs.
- The proxy adds/removes only the Cloud Code transport envelope, with one backend-specific schema adaptation: `generationConfig.responseJsonSchema` is renamed to `generationConfig.responseSchema` so Bifrost structured-output requests reach the backend. Schema types and constraints are unchanged. Supplying both schema fields returns a native `400` error.
- There is no prompt injection, output cleaning, schema filtering, or automatic token/thinking-budget default or cap. Catalog token metadata is descriptive, not a proxy request clamp. The transport limits are a **50 MiB request body**, a **five-minute response-header deadline**, a **resetting five-minute body-inactivity deadline**, and **two concurrent generations** by default; active responses have no fixed lifetime cap. Upstream model limits still apply.
- Successful SSE events are unwrapped and flushed incrementally, with one complete JSON value per `data:` line. Upstream HTTP errors preserve their status and validated retry headers but use a sanitized Gemini error envelope instead of exposing arbitrary provider bodies. Malformed upstream data before streaming starts returns `502`; a broken stream or EOF before started candidates finish aborts the connection rather than fabricating successful completion. Native error and blocked-prompt outcomes remain valid terminal responses. Clients must treat an interrupted stream as incomplete.
- Daily-to-production fallback is limited to transport failures, `404`, and `5xx`, before a successful upstream response. Once a successful upstream response begins, the request is not replayed against another endpoint.

Not implemented: Anthropic Messages or OpenAI endpoints, `countTokens`, embeddings, Imagen `predict`, Veo, file uploads, caches, batches, or audio-special endpoints. Unsupported model actions return a native `501 UNIMPLEMENTED` error; unrelated paths return `404`. A model appearing in the catalog does not add those operations.

### Video understanding

Send a video through `generateContent` as a native content part with `inlineData.mimeType: "video/mp4"` and `inlineData.data` containing the base64-encoded MP4 bytes. Put the question in a separate text part of the same content entry. The proxy forwards the video without extracting frames or transcoding it. The 50 MiB request-body limit includes base64 expansion and JSON overhead; file uploads are not implemented. Sending a video URL as ordinary text is not equivalent to sending video bytes.

## Quota status and history

`serve` polls the same Google account used for generation through the internal `retrieveUserQuotaSummary` endpoint: immediately on startup, then every **300 seconds (five minutes)** by default. Set `quotaPollIntervalSeconds` or `ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS` to a positive integer number of seconds; zero does not disable polling. One worker polls without overlapping requests. Status reads use the persisted cache and **do not fetch upstream or add observations**. Neither `login` nor help starts polling. An installed Antigravity CLI is not a runtime dependency.

Both status routes require the same local `API_KEY` when configured, return raw status JSON (not a Gemini RPC envelope), and send `Cache-Control: no-store`. Only `GET` is supported; other methods return `405`.

```sh
curl "$ANTIGRAVITY_PROXY_URL/status/limit" \
  -H "x-api-key: $ANTIGRAVITY_PROXY_API_KEY"

# Illustrative UTC range; curl URL-encodes each query value.
curl --get "$ANTIGRAVITY_PROXY_URL/status/usage" \
  -H "x-api-key: $ANTIGRAVITY_PROXY_API_KEY" \
  --data-urlencode 'from=2026-01-01T00:00:00Z' \
  --data-urlencode 'to=2026-01-02T00:00:00Z' \
  --data-urlencode 'pool=third_party' \
  --data-urlencode 'window=weekly' \
  --data-urlencode 'limit=100' \
  --data-urlencode 'offset=0' \
  --data-urlencode 'order=desc'
```

Illustrative `/status/limit` response only; these dates and values are not account data or promised allowances:

```json
{
  "observed_at": "2026-01-01T12:00:00Z",
  "source": "retrieveUserQuotaSummary",
  "pools": {
    "gemini": {
      "id": "gemini", "display_name": "Gemini",
      "five_hour": {
        "bucket_id": "gemini-5h", "window": "5h", "status": "available", "disabled": false,
        "remaining_fraction": 0.6, "remaining_percent": 60, "used_percent": 40,
        "remaining_amount": null, "reset_at": "2026-01-01T15:00:00Z"
      },
      "weekly": {
        "bucket_id": "gemini-weekly", "window": "weekly", "status": "available", "disabled": false,
        "remaining_fraction": 0.8, "remaining_percent": 80, "used_percent": 20,
        "remaining_amount": null, "reset_at": "2026-01-05T12:00:00Z"
      }
    },
    "third_party": {
      "id": "third_party", "display_name": "Claude / GPT / other",
      "five_hour": {
        "bucket_id": "3p-5h", "window": "5h", "status": "available", "disabled": false,
        "remaining_fraction": 0.4, "remaining_percent": 40, "used_percent": 60,
        "remaining_amount": null, "reset_at": "2026-01-01T16:00:00Z"
      },
      "weekly": {
        "bucket_id": "3p-weekly", "window": "weekly", "status": "available", "disabled": false,
        "remaining_fraction": 0.7, "remaining_percent": 70, "used_percent": 30,
        "remaining_amount": null, "reset_at": "2026-01-06T12:00:00Z"
      }
    }
  },
  "polling": {
    "interval_seconds": 300,
    "last_attempt_at": "2026-01-01T12:00:00Z",
    "last_success_at": "2026-01-01T12:00:00Z"
  },
  "stale": false
}
```

All four windows are represented. `status` is `available`, `disabled`, or `unavailable`; disabled or missing/invalid upstream data has unusable values represented as `null` and an `unavailable_reason` (for example, `upstream_disabled` or `not_reported`). A valid remaining fraction of zero means exhausted quota, not missing data. Percentages derive only from a valid upstream fraction; `remaining_amount`, if supplied, is a decimal string without invented units. Reset times come only from upstream and may be `null`. The proxy does not infer weekly limits from model metadata or fabricate refresh times.

`stale` is true when the observation is older than twice the configured interval or the latest polling attempt failed. The grace interval avoids marking data stale while a scheduled fetch completes. `polling` reports the interval, nullable attempt/success timestamps, and `last_error` when present. Before any successful persisted observation, `/status/limit` returns `503` with an error and polling metadata. A failed fetch or save retains the last good snapshot, marked stale; a restart reloads persisted history before the next fetch completes.

### History queries

| Parameter | Accepted values | Default |
| --- | --- | --- |
| `from` | Inclusive observation-time lower bound; RFC3339 with timezone | no lower bound |
| `to` | Inclusive observation-time upper bound; RFC3339 with timezone, not before `from` | no upper bound |
| `pool` | `all`, `gemini`, `third_party` | `all` |
| `window` | `all`, `5h`, `weekly` | `all` |
| `limit` | Integer `1`–`1000` | `100` |
| `offset` | Integer `0` or greater | `0` |
| `order` | `asc`, `desc` by observation time | `desc` |

Unknown or duplicate parameters, empty values, and invalid values return `400`. When using query-key authentication, `key` is also accepted as the local API key. Fix a `to` timestamp across pages to exclude new polls; retention can still evict older entries and shift offsets, so pagination is not a frozen snapshot.

`/status/usage` returns `entries`, `total` (matching window entries before pagination), `limit`, `offset`, nullable `next_offset`, `order`, selected `pool`/`window`, optional `from`/`to`, and `polling`. Each entry is one selected window from one observation: `observed_at`, `pool`, `pool_display_name`, plus the flat window fields shown above. Thus one poll contributes up to four entries, including disabled/unavailable windows with null values. An empty result has `entries: []`; follow `next_offset` until it is `null`.

This is **quota observation history, not billable usage**. `used_percent` describes the consumed share of the current upstream quota bucket; it is not a count of proxy requests, tokens, or credits. History is not resampled, and differences between snapshots are not reported as consumption: resets and other account activity can change quota between polls.

### Persistence and account scope

Observations are stored in owner-only JSONL at `$HOME/.local/share/antigravity-proxy/usage.jsonl` by default, independently of the OAuth config. Override with `quotaHistoryPath` or nonempty `ANTIGRAVITY_QUOTA_HISTORY_PATH`. Each successful observation is synced before publication. An incomplete final record after a crash is truncated with a warning; malformed complete records and inaccessible storage still fail startup. SIGINT/SIGTERM stop polling and close storage.

History retains the latest **10,000 observations** by default (about 35 days at five-minute polling). Set positive `quotaHistoryMaxSamples` or `ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES` to change this bound. Startup compacts older observations; ongoing collection bounds disk and memory too. An exclusive companion lock enforces **one writer per history path**, including during file replacement. The file is not account-partitioned: choose a separate history path/volume when changing Google credentials. Protect and back up history as account-related data; it does not contain OAuth tokens.

Strict count retention rewrites the retained observations when the file is full: encoding and disk writes are proportional to `quotaHistoryMaxSamples` on each subsequent successful poll. This favors an exact disk bound and atomic recovery over append-only write efficiency. Larger retention counts or shorter poll intervals increase write volume; choose them according to storage capacity and durability requirements.

A local Linux/amd64 measurement using unavailable-window snapshots, ten durable rewrites per case, measured approximately 14 ms / 1.15 MB per rewrite at 1,000 observations and 161 ms / 11.49 MB at 10,000. These are filesystem/workload-specific observations, not performance guarantees; five-minute polling at the latter size rewrites about 3.3 GB/day once full.

Parent-directory symlink aliases resolve to one history/lock identity. The history file and its companion lock cannot themselves be symlinks; use a direct regular-file path. Startup removes abandoned compaction files belonging to that history under its lock. Legacy anonymous `.quota-history-*` files cannot be safely attributed and require manual inspection rather than automatic deletion.

History locking/durability implementations cover Linux/Android, macOS/iOS, DragonFly/FreeBSD/NetBSD/OpenBSD, illumos, Solaris, AIX and Windows. Unix storage uses real file locks plus rename/directory sync; Windows uses `LockFileEx` and write-through `MoveFileEx`. Writable non-append history handles support torn-tail truncation on Windows; appends seek to EOF under the exclusive lock. Other targets fail closed instead of silently omitting storage locking.

For authoritative product policy and model availability, see Google's [Antigravity Plans](https://antigravity.google/docs/plans), [Models](https://antigravity.google/docs/models), and [CLI model quotas (`/usage`)](https://antigravity.google/docs/cli/commands/usage/). Those pages describe the product, not a supported public quota API contract. This proxy uses an internal endpoint, and quotas/availability can change upstream.


## Project layout

```text
cmd/antigravity-proxy/main.go   CLI entry point, service composition, and shutdown
internal/config/              Configuration loading, validation, and persistence
internal/oauth/               Google OAuth login and token exchange
internal/proxy/               Shared Google transport/caches, authentication, and native Gemini routing
internal/quota/               Quota fetching, request identity, safe errors, and snapshot parsing
internal/status/              Status HTTP API, polling lifecycle, durable history, and queries
go.mod                        Module definition
```

Tests live alongside the package they exercise. `cmd/` contains the executable; `internal/` packages are implementation details, not a public library API. Keep new code with the package that owns its behavior, and introduce another package only when it has a distinct responsibility.

The command explicitly composes the services: `proxy.New(cfg)` owns the shared Google account transport; `Proxy.QuotaFetcher()` connects it to `quota.Fetcher`; `status.NewService(cfg, fetcher.Fetch)` owns the collector and status endpoints. Wire the service into `Proxy.StatusHandler` before serving, then start and close it from the command lifecycle. Proxy authenticates status requests before delegation but does not own polling or history state. Quota polling reuses the native token/project caches without initiating project discovery or waiting on unrelated refresh/discovery locks.

Run all package tests and static checks from the repository root:

```sh
go test -race -count=1 ./...
go vet ./...
gofmt -l cmd internal
```

CI runs formatting, vet and race checks before building or publishing Docker images.

The root no longer contains a Go executable package; use `./cmd/antigravity-proxy` for build/run commands.

## Configuration

The optional JSON file is `~/.config/antigravity-proxy/config.json`:

```json
{
  "port": 8080,
  "host": "127.0.0.1",
  "apiKey": "",
  "refreshToken": "",
  "oauthClientId": "",
  "oauthClientSecret": "",
  "projectId": "",
  "quotaPollIntervalSeconds": 300,
  "quotaHistoryMaxSamples": 10000,
  "maxConcurrentGenerations": 2,
  "clientVersion": "1.15.8",
  "quotaHistoryPath": "/home/your-user/.local/share/antigravity-proxy/usage.jsonl"
}
```

The history path above is illustrative: replace it with a writable path for your runtime user, or omit `quotaHistoryPath` to use `$HOME/.local/share/antigravity-proxy/usage.jsonl`. JSON paths do not expand `$HOME`. The Docker image explicitly sets `ANTIGRAVITY_QUOTA_HISTORY_PATH` to `/home/app/.local/share/antigravity-proxy/usage.jsonl`, which takes precedence over JSON; a custom container path requires a runtime environment override and a matching writable mount. `quotaPollIntervalSeconds` must be a positive integer; its default is `300`.

Environment variables override file values:

| Variable | Purpose | Default |
| --- | --- | --- |
| `HOST` | Listen address | `127.0.0.1` |
| `PORT` | Listen port | `8080` |
| `API_KEY` | Protect `/models`, `/status/limit`, `/status/usage`, and all `/v1beta` endpoints; unset disables API-key auth (`/health` stays public) | unset |
| `ANTIGRAVITY_ACCESS_TOKEN` | Use a supplied access token instead of refreshing OAuth | unset |
| `ANTIGRAVITY_REFRESH_TOKEN` | Google OAuth refresh token | config file / login |
| `ANTIGRAVITY_OAUTH_CLIENT_ID` | Complete-pair override for the issuing Google OAuth client | saved config; automatic discovery during login |
| `ANTIGRAVITY_OAUTH_CLIENT_SECRET` | Complete-pair override for the issuing Google OAuth client secret | saved config; automatic discovery during login |
| `ANTIGRAVITY_PROJECT_ID` | Explicit Cloud Code project ID | auto-discovery |
| `ANTIGRAVITY_DAILY_ENDPOINT` | Daily Cloud Code endpoint override | `https://daily-cloudcode-pa.googleapis.com` |
| `ANTIGRAVITY_PROD_ENDPOINT` | Production Cloud Code endpoint override | `https://cloudcode-pa.googleapis.com` |
| `ANTIGRAVITY_CLIENT_VERSION` | Client version sent in Antigravity headers | `1.15.8` |
| `OAUTH_CALLBACK_PORT` | Preferred IPv4 loopback OAuth callback port; fallback ports are tried if busy | `51121` |
| `ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS` | Positive integer polling interval in seconds; zero is invalid | `300` |
| `ANTIGRAVITY_QUOTA_HISTORY_PATH` | Nonempty writable JSONL history path, separate from the OAuth config | `$HOME/.local/share/antigravity-proxy/usage.jsonl`; image explicitly sets `/home/app/.local/share/antigravity-proxy/usage.jsonl` |
| `ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES` | Positive maximum retained quota observations | `10000` |
| `ANTIGRAVITY_MAX_CONCURRENT_GENERATIONS` | Positive concurrent generation limit; saturation returns `429` before reading the body | `2` |

Cloud Code endpoint overrides must use HTTPS. Plain HTTP is accepted only for `localhost` or loopback addresses, so local stub servers can be used without sending tokens over a network connection.

When a refresh token is configured, the service refreshes and caches its Google access token as needed using the saved or explicitly supplied client pair. A directly supplied access token bypasses client discovery/resolution and is not refreshed automatically. Configuration files created by `login`, including the client pair and account tokens, are written with mode `0600`; do not commit or bake them into images.

Cloud Code calls use daily then production consistently. Fallback is limited to transport failures, HTTP `404`, and `5xx`; `400`, auth failures, and `429` are terminal. Safe retry headers, including valid `Retry-After`, survive error responses; arbitrary upstream/OAuth error bodies are not exposed. Streaming requests are never replayed after a successful upstream response. Project discovery errors are explicit rather than silently selecting a shared fallback project. Discovered project IDs are invalidated only after terminal, narrowly classified project-specific `403`/`404` errors; a successful production fallback or generic permission denial preserves project/catalog caches. The service supports one Google account and does not automatically retry generation after quota exhaustion.

Generation has no fixed total duration limit: connection setup, request upload and waiting for upstream response headers share a five-minute deadline; body inactivity has a resetting five-minute deadline, while active streams can continue. Finite quota, discovery, onboarding and model-catalog operations retain a five-minute total deadline including body reads and endpoint fallback. Error classification uses bounded reads and never exposes provider messages or details. Caller cancellation closes upstream work. Generation envelopes stream small metadata around the validated raw request rather than copying the complete body through JSON marshaling. The 50 MiB request limit remains; concurrent generation capacity bounds aggregate request-memory pressure. Saturation returns `429` with `Retry-After: 1` and closes HTTP/1 connections without draining incomplete request bodies. Model discovery uses a five-minute catalog cache shared by listing and lookup, with project invalidation and independent waiter cancellation.

The executable emits structured JSON logs through `log/slog`. Access events include route, model, status, duration, request ID and upstream endpoint, without query strings, credentials or request/response bodies. `X-Request-ID` correlates client responses with logs and generation envelopes. Generation, discovery and quota use one configured client version; the proxy does not send a fabricated `x-goog-api-client` identity. This does not eliminate the account risk of using an unofficial internal API.
