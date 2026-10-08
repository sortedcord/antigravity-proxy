# API Reference

Antigravity Proxy exposes a native Google Gemini `/v1beta` HTTP interface along with quota monitoring and health endpoints.

## Endpoints Overview

| Method and Path | Description | Authentication |
|---|---|---|
| `GET /health` | Health check & credential configuration status | None (Public) |
| `GET /models` | Raw Cloud Code `fetchAvailableModels` JSON catalog | Local `API_KEY` |
| `GET /status/limit` | Latest persisted quota snapshot across model pools | Local `API_KEY` |
| `GET /status/usage` | Historical quota observations with filtering & pagination | Local `API_KEY` |
| `GET /status/account` | Authenticated Google account details (email, name, subscription tier) | Local `API_KEY` |
| `GET /config/login` | Initiate runtime OAuth login; returns Google authorization URL | Local `API_KEY` |
| `POST /config/login` | Complete runtime OAuth login with code/state or redirect URL | Local `API_KEY` |
| `GET /v1beta/models` | Native Gemini model list with keyset pagination | Local `API_KEY` |
| `GET /v1beta/models/{id}` | Native Gemini model details | Local `API_KEY` |
| `POST /v1beta/models/{id}:generateContent` | Native Gemini content generation | Local `API_KEY` |
| `POST /v1beta/models/{id}:streamGenerateContent?alt=sse` | Native Gemini streaming generation (SSE) | Local `API_KEY` |

---

## Authentication

When `API_KEY` is set on the server, requests to protected routes must include the key via one of the following methods (evaluated in priority order):

1. `x-goog-api-key: <API_KEY>` (standard Gemini header)
2. `?key=<API_KEY>` query parameter
3. `x-api-key: <API_KEY>`
4. `Authorization: Bearer <API_KEY>`

If no `API_KEY` is configured on the server, API key authentication is disabled for local loopback use. The local API key authenticates clients to the proxy; Google Cloud Code upstream authentication is handled securely by the proxy using its stored Google OAuth credentials.

`/config/login` is an administration exception: the server must have a configured `API_KEY`, even on loopback. Without it, both login methods return `503`; a missing or incorrect request key returns `401`. Use headers rather than URL query parameters, and HTTPS or an encrypted tunnel for remote setup. The same key permits account replacement, so distribute it only to trusted administrators if management routes are reachable.

The server may start without Google credentials. Model discovery and generation reject those requests locally with `503` before body reads, generation capacity acquisition, token refresh, project discovery, or Google calls. `/health` stays public and reports local credential presence, not proof that Google credentials remain valid.

---

## Model Discovery

### `GET /v1beta/models`

Lists all available external generative models dynamically discovered from Google Cloud Code. The list includes Gemini, Claude, and GPT models configured for your account.

**Query Parameters:**
- `pageSize`: Optional integer (1–1000, default: 50).
- `pageToken`: Keyset cursor from a previous `nextPageToken`.

**Example:**
```sh
curl http://127.0.0.1:8080/v1beta/models?pageSize=100 \
  -H "x-goog-api-key: $API_KEY"
```

**Response Format:**
```json
{
  "models": [
    {
      "name": "models/gemini-3.8-flash-tiered",
      "displayName": "Gemini 3.8 Flash (Tiered)",
      "supportedGenerationMethods": ["generateContent", "streamGenerateContent"]
    }
  ],
  "nextPageToken": "..."
}
```

---

## Content Generation

### `POST /v1beta/models/{id}:generateContent`

Generates content synchronously using the native Gemini request/response envelope.

**Example Request:**
```sh
curl http://127.0.0.1:8080/v1beta/models/gemini-3.8-flash-tiered:generateContent \
  -H "x-goog-api-key: $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [
      {
        "role": "user",
        "parts": [{"text": "Explain quantum computing in one sentence."}]
      }
    ]
  }'
```

### `POST /v1beta/models/{id}:streamGenerateContent?alt=sse`

Streams responses incrementally using Server-Sent Events (SSE). The `alt=sse` query parameter is required.

**Example Request:**
```sh
curl --no-buffer "http://127.0.0.1:8080/v1beta/models/gemini-3.8-flash-tiered:streamGenerateContent?alt=sse" \
  -H "x-goog-api-key: $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [
      {
        "role": "user",
        "parts": [{"text": "Count from 1 to 5."}]
      }
    ]
  }'
```

Each event is delivered as a JSON string prefixed with `data: `, followed by a blank line.

### Multimodal & Video Understanding

Videos and images are passed as native content parts with base64-encoded `inlineData`:

```json
{
  "contents": [
    {
      "role": "user",
      "parts": [
        {
          "inlineData": {
            "mimeType": "video/mp4",
            "data": "<base64_encoded_mp4>"
          }
        },
        {
          "text": "Summarize what happens in this clip."
        }
      ]
    }
  ]
}
```
*Note: Request bodies are capped at 50 MiB, including base64 expansion.*

---

## Quota & Rate Limit Status

### `GET /status/limit`

Returns the latest observed snapshot of your Antigravity quotas. Data is polled in the background from Google's `retrieveUserQuotaSummary` endpoint.

**Example:**
```sh
curl http://127.0.0.1:8080/status/limit \
  -H "x-goog-api-key: $API_KEY"
```

**Response Format:**
```json
{
  "observed_at": "2026-10-07T09:59:38Z",
  "source": "retrieveUserQuotaSummary",
  "pools": {
    "gemini": {
      "id": "gemini",
      "display_name": "Gemini Models",
      "five_hour": {
        "bucket_id": "gemini-5h",
        "window": "5h",
        "status": "available",
        "disabled": false,
        "remaining_fraction": 0.998,
        "remaining_percent": 99.8,
        "used_percent": 0.2,
        "reset_at": "2026-10-07T12:20:15Z"
      },
      "weekly": {
        "bucket_id": "gemini-weekly",
        "window": "weekly",
        "status": "available",
        "disabled": false,
        "remaining_fraction": 0.998,
        "remaining_percent": 99.8,
        "used_percent": 0.2,
        "reset_at": "2026-10-11T16:02:27Z"
      }
    },
    "third_party": {
      "id": "third_party",
      "display_name": "Claude and GPT models",
      "five_hour": {
        "bucket_id": "3p-5h",
        "window": "5h",
        "status": "available",
        "disabled": false,
        "remaining_fraction": 0.931,
        "remaining_percent": 93.1,
        "used_percent": 6.9,
        "reset_at": "2026-10-07T14:18:50Z"
      },
      "weekly": {
        "bucket_id": "3p-weekly",
        "window": "weekly",
        "status": "available",
        "disabled": false,
        "remaining_fraction": 0.953,
        "remaining_percent": 95.3,
        "used_percent": 4.7,
        "reset_at": "2026-10-13T04:42:09Z"
      }
    }
  },
  "polling": {
    "interval_seconds": 300,
    "last_attempt_at": "2026-10-07T09:59:37Z",
    "last_success_at": "2026-10-07T09:59:38Z"
  },
  "stale": false
}
```

### `GET /status/usage`

Queries durable historical quota observations persisted in `usage.jsonl`.

**Query Parameters:**
| Parameter | Accepted Values | Default |
|---|---|---|
| `from` | RFC3339 timestamp with timezone | None |
| `to` | RFC3339 timestamp with timezone | None |
| `pool` | `all`, `gemini`, `third_party` | `all` |
| `window` | `all`, `5h`, `weekly` | `all` |
| `limit` | Integer `1`–`1000` | `100` |
| `offset` | Integer `0` or greater | `0` |
| `order` | `asc`, `desc` by observation timestamp | `desc` |

**Example:**
```sh
curl --get "http://127.0.0.1:8080/status/usage" \
  -H "x-goog-api-key: $API_KEY" \
  --data-urlencode "pool=third_party" \
  --data-urlencode "window=weekly" \
  --data-urlencode "limit=10"
```

### `GET /status/account`

Returns details about the configured Google account including email, display name, and active subscription tier. Fails fast with `503` if no account has been configured.

**Example:**
```sh
curl http://127.0.0.1:8080/status/account \
  -H "x-goog-api-key: $API_KEY"
```

**Response Format:**
```json
{
  "credential_configured": true,
  "email": "developer@example.com",
  "name": "Example Developer",
  "subscription": {
    "id": "g1-pro-tier",
    "display_name": "Google AI Pro",
    "source_field": "paidTier"
  },
  "subscription_status": "known"
}
```

The profile is bound to the active account snapshot. Subscription metadata prefers `paidTier`, then `currentTier`; the latter is an access tier, not independent proof of a paid billing plan. Missing provider tier metadata produces `subscription: null` and `subscription_status: "not_reported"`; lookup failures produce `"unavailable"` without hiding a known profile. No account onboarding is performed to display account details.

Quota observations are stored in account-specific journals with hashed identity suffixes. Successful login saves account identity with credentials so restart reopens the same journal. Unlabelled legacy `usage.jsonl` is retained untouched, never attributed to another account. Environment token overrides discard saved profile attribution; read-only identity lookup resolves those credentials before account quota polling begins.

---

## Runtime Account Configuration

### `GET /config/login`

Initiates a runtime Google OAuth login session and returns the authorization URL.

Login sessions use state validation and PKCE and expire after five minutes. Repeated GET requests reuse a pending attempt; completion already in progress returns `409`. Abandoned, failed, expired, and completed attempts release their callback listeners. Failed attempts are terminal: start a new GET instead of replaying a consumed code. Unsupported methods return `405`.

**Example:**
```sh
curl http://127.0.0.1:8080/config/login \
  -H "x-goog-api-key: $API_KEY"
```

**Response Format:**
```json
{
  "login_url": "https://accounts.google.com/o/oauth2/v2/auth?...",
  "expires_at": "2026-10-07T16:30:00Z"
}
```

### `POST /config/login`

Completes runtime Google OAuth login. When authenticating from a browser on a different machine, the user can submit the authorization code and state (or the full redirected URL) from the browser address bar.

On the browser's machine, `127.0.0.1` refers to that machine, not a remote proxy or container. When the loopback redirect cannot connect, copy its URL from the address bar and submit it to the proxy. An SSH tunnel can instead deliver the callback automatically. POST exchanges and persists credentials on the proxy, then publishes a new complete account snapshot; the old account remains usable until this commit. Existing automatic token refresh is preserved.

Use either `code` plus `state`, or the `url` field. Do not combine them. The JSON body is bounded to 64 KiB. Identity, history preparation, or credential-save failure before commit leaves the old account and saved credentials intact. Invalid state returns `400`; expired state `410`; replay or concurrent completion `409`; Google exchange/profile failures `502`; local preparation/save failures `500`.

If file replacement succeeds but directory sync fails, POST returns `500` with `credential_configured: true` and `durability_uncertain: true`. The live account matches the replacement on disk; this is not an unchanged-account failure. The response contains no tokens or filesystem causes. Start a fresh login/save to confirm durability.

**Payload Options:**
```json
{
  "code": "4/0A...",
  "state": "a1b2..."
}
```
or with the full redirect URL:
```json
{
  "url": "http://127.0.0.1:51121/oauth-callback?state=a1b2...&code=4/0A..."
}
```

**Response Format:**
```json
{
  "status": "configured",
  "email": "developer@example.com",
  "name": "Example Developer"
}
```
