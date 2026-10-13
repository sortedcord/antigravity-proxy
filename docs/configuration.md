# Configuration Guide

Antigravity Proxy can be configured via a JSON configuration file, environment variables, or a combination of both. Environment variables take precedence over file-based values.

---

## Configuration File

The default configuration file is located at:
```text
~/.config/antigravity-proxy/config.json
```

On Unix systems, this file must have owner-only permissions (`chmod 600`). The proxy rejects files that are world- or group-readable.

### Full Schema & Example

```json
{
  "host": "127.0.0.1",
  "port": 8080,
  "adminKeyHash": "sha256:d8a2...3f1c",
  "accounts": [
    {
      "id": "work",
      "label": "Work Account",
      "enabled": true,
      "refreshToken": "1//04...",
      "oauthClientId": "107100...",
      "oauthClientSecret": "GOCSPX-...",
      "accountId": "1123456789...",
      "accountEmail": "developer@company.com",
      "accountName": "Jane Developer",
      "projectId": "",
      "keys": [
        {
          "id": "key_3f9a1c2e",
          "label": "bifrost",
          "hash": "sha256:4b8e...",
          "last4": "2m3f",
          "createdAt": "2026-10-13T12:00:00Z"
        }
      ]
    }
  ],
  "clientVersion": "1.15.8",
  "maxConcurrentGenerations": 2,
  "quotaPollIntervalSeconds": 300,
  "quotaHistoryMaxSamples": 10000,
  "quotaHistoryPath": "/home/your-user/.local/share/antigravity-proxy/usage.jsonl",
  "dailyEndpoint": "https://daily-cloudcode-pa.googleapis.com",
  "prodEndpoint": "https://cloudcode-pa.googleapis.com"
}
```

### Migration from Single-Account Configurations
Antigravity Proxy automatically migrates existing single-account configurations on load. If your configuration file contains flat `refreshToken`, `apiKey`, and `accountId` fields without an `accounts` array:
- It is converted in-memory into a single account named `default`.
- Your existing `apiKey` becomes its first key (`key_legacy`).
- The next administrative update (e.g. adding a new account, rotating keys) rewrites the file in the new multi-account format and strips the legacy flat fields.

---

## Environment Variables

Every setting can be specified or overridden using environment variables:

| Environment Variable | Description | Default |
|---|---|---|
| `HOST` | Network interface to bind to (`127.0.0.1`, `0.0.0.0`, etc.) | `127.0.0.1` (Binary) / `0.0.0.0` (Docker) |
| `PORT` | Listening port for the HTTP server | `8080` |
| `ADMIN_API_KEY` | Secret key for `/admin/*` management routes and CLI | Unset |
| `API_KEY` | Legacy/default account client authentication key | Unset |
| `ANTIGRAVITY_ACCESS_TOKEN` | Direct short-lived Google access token (bypasses OAuth refresh) | Unset |
| `ANTIGRAVITY_REFRESH_TOKEN` | Google OAuth refresh token | Saved in config |
| `ANTIGRAVITY_OAUTH_CLIENT_ID` | OAuth Client ID paired with the refresh token | Auto-discovered / Saved |
| `ANTIGRAVITY_OAUTH_CLIENT_SECRET` | OAuth Client Secret paired with the refresh token | Auto-discovered / Saved |
| `ANTIGRAVITY_PROJECT_ID` | Cloud Code project ID (auto-discovered if empty) | Auto-discovered |
| `ANTIGRAVITY_CLIENT_VERSION` | Antigravity client version advertised upstream | `1.15.8` |
| `ANTIGRAVITY_MAX_CONCURRENT_GENERATIONS` | Maximum parallel in-flight generation requests | `2` |
| `ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS` | Interval between background quota polls (in seconds) | `300` |
| `ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES` | Maximum quota observation entries retained | `10000` |
| `ANTIGRAVITY_QUOTA_HISTORY_PATH` | Path to persistent quota history JSONL file | `~/.local/share/antigravity-proxy/usage.jsonl` |
| `ANTIGRAVITY_DAILY_ENDPOINT` | Daily Cloud Code endpoint override | `https://daily-cloudcode-pa.googleapis.com` |
| `ANTIGRAVITY_PROD_ENDPOINT` | Production Cloud Code endpoint override | `https://cloudcode-pa.googleapis.com` |
| `OAUTH_CALLBACK_PORT` | Local port for browser OAuth callback | `51121` (falls back to 51122–51126) |

---

## Security & Access Control

1. **Non-Loopback Listener Safeguard**:
   If `HOST` is configured to `0.0.0.0` or any non-loopback IP, the proxy will refuse to start unless an `API_KEY` is provided. This prevents accidental public exposure of your Antigravity account.

2. **Owner-Only Permissions**:
   The proxy strictly verifies permissions on `config.json` (`0600`) to ensure local users cannot access stored Google credentials.

3. **Isolated Client Authentication**:
   The `API_KEY` you define only authenticates clients to this proxy (e.g., Bifrost, curl, or your coding agent). It is never forwarded to Google. Google requests use the OAuth bearer token obtained via Google OAuth.

4. **Structured Logging**:
   All logs use Go's standard `log/slog` in structured JSON format. Credentials, prompts, responses, and authorization tokens are never logged.
