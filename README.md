# Antigravity Proxy (Go)

A single-account bridge from the native Gemini API to Antigravity Cloud Code, using server-side Google OAuth. It exposes model discovery, `generateContent`, and SSE streaming so Bifrost can use its native Gemini provider. It does not expose Anthropic or OpenAI API endpoints.

> **Account risk:** This is an unofficial integration with internal Cloud Code endpoints. Google may enforce its terms, including account suspension. Use an account you can afford to lose. This project is not endorsed by Google.

## Build and run

Requires Go 1.22 or later.

```sh
go build -o antigravity-proxy ./cmd/antigravity-proxy
./antigravity-proxy login
./antigravity-proxy serve
```

`login` prints Google's authorization URL and waits for a loopback OAuth callback (port `51121` by default, with fallback ports if busy). The resulting refresh token is stored in `~/.config/antigravity-proxy/config.json` with owner-only file permissions. You can use `go run ./cmd/antigravity-proxy login` and `go run ./cmd/antigravity-proxy serve` instead.

OAuth login and refresh-token exchange require a Google OAuth client ID and client secret provided as `ANTIGRAVITY_OAUTH_CLIENT_ID` and `ANTIGRAVITY_OAUTH_CLIENT_SECRET`. Set these in the environment for `login` and for any service process that must refresh an access token. They are intentionally not stored in the repository or config file.

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

Startup rejects a non-loopback `HOST` unless `API_KEY` is set. When configured, the local API key protects `/models` and every `/v1beta` endpoint. Send it in `x-api-key` or `Authorization: Bearer ...`; native Gemini endpoints also accept `x-goog-api-key` or the `key` query parameter. An unset key disables API-key authentication. `/health` remains unauthenticated and does not disclose credentials. This local key is never sent upstream: Cloud Code uses the service's Google OAuth credential.

## Endpoints

| Method and path | Behavior |
| --- | --- |
| `GET /health` | Local health and whether a Google credential is configured; unauthenticated |
| `GET /models` | Raw Cloud Code `fetchAvailableModels` JSON, unchanged |
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
- There is no prompt injection, output cleaning, schema filtering, or automatic token/thinking-budget default or cap. Catalog token metadata is descriptive, not a proxy request clamp. The existing transport limits are a **50 MiB request body** and a **five-minute upstream timeout**; upstream model limits still apply.
- Successful SSE events are unwrapped and flushed incrementally, with one complete JSON value per `data:` line. Upstream HTTP errors preserve the Google error object and status. Malformed upstream data before streaming starts returns `502`; a broken stream or EOF before started candidates finish aborts the connection rather than fabricating a successful completion. Native error and blocked-prompt outcomes remain valid terminal responses. Clients must treat an interrupted stream as incomplete.
- Daily-to-production fallback happens only before a successful response. Once successful SSE streaming begins, the request is not replayed against another endpoint.

Not implemented: Anthropic Messages or OpenAI endpoints, `countTokens`, embeddings, Imagen `predict`, Veo, file uploads, caches, batches, or audio-special endpoints. Unsupported model actions return a native `501 UNIMPLEMENTED` error; unrelated paths return `404`. A model appearing in the catalog does not add those operations.

### Video understanding

Send a video through `generateContent` as a native content part with `inlineData.mimeType: "video/mp4"` and `inlineData.data` containing the base64-encoded MP4 bytes. Put the question in a separate text part of the same content entry. The proxy forwards the video without extracting frames or transcoding it. The 50 MiB request-body limit includes base64 expansion and JSON overhead; file uploads are not implemented. Sending a video URL as ordinary text is not equivalent to sending video bytes.

## Project layout

```text
cmd/antigravity-proxy/main.go   CLI entry point and HTTP server startup
internal/config/              Configuration loading, validation, and persistence
internal/oauth/               Google OAuth login and token exchange
internal/proxy/               Antigravity discovery and native Gemini HTTP bridge
go.mod                        Module definition
```

Tests live alongside the package they exercise. `cmd/` contains the executable; `internal/` packages are implementation details, not a public library API. Keep new code with the package that owns its behavior, and introduce another package only when it has a distinct responsibility.

Run all package tests and static checks from the repository root:

```sh
go test ./...
go vet ./...
```

The root no longer contains a Go executable package; use `./cmd/antigravity-proxy` for build/run commands.

## Configuration

The optional JSON file is `~/.config/antigravity-proxy/config.json`:

```json
{
  "port": 8080,
  "host": "127.0.0.1",
  "apiKey": "",
  "refreshToken": "",
  "projectId": ""
}
```

Environment variables override file values:

| Variable | Purpose | Default |
| --- | --- | --- |
| `HOST` | Listen address | `127.0.0.1` |
| `PORT` | Listen port | `8080` |
| `API_KEY` | Protect `/models` and all `/v1beta` endpoints; unset disables API-key auth (`/health` stays public) | unset |
| `ANTIGRAVITY_ACCESS_TOKEN` | Use a supplied access token instead of refreshing OAuth | unset |
| `ANTIGRAVITY_REFRESH_TOKEN` | Google OAuth refresh token | config file / login |
| `ANTIGRAVITY_OAUTH_CLIENT_ID` | Google OAuth client ID used for login and refresh | required for OAuth login/refresh |
| `ANTIGRAVITY_OAUTH_CLIENT_SECRET` | Google OAuth client secret used for login and refresh | required for OAuth login/refresh |
| `ANTIGRAVITY_PROJECT_ID` | Explicit Cloud Code project ID | auto-discovery |
| `ANTIGRAVITY_DAILY_ENDPOINT` | Daily Cloud Code endpoint override | `https://daily-cloudcode-pa.googleapis.com` |
| `ANTIGRAVITY_PROD_ENDPOINT` | Production Cloud Code endpoint override | `https://cloudcode-pa.googleapis.com` |
| `ANTIGRAVITY_CLIENT_VERSION` | Client version sent in Antigravity headers | `1.15.8` |
| `OAUTH_CALLBACK_PORT` | Preferred localhost OAuth callback port; fallback ports are tried if busy | `51121` |

Cloud Code endpoint overrides must use HTTPS. Plain HTTP is accepted only for `localhost` or loopback addresses, so local stub servers can be used without sending tokens over a network connection.

When a refresh token is configured, the service refreshes and caches its Google access token as needed. A directly supplied access token is not refreshed automatically. Configuration files created by `login` are written with mode `0600`.

Cloud Code requests fall back from the daily endpoint to production when the first endpoint fails before a successful response; streaming requests are never replayed after successful SSE begins. The service supports one Google account and does not include quota tracking or an automatic retry/cooldown policy.
