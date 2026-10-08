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
  "apiKey": "",
  "refreshToken": "",
  "oauthClientId": "",
  "oauthClientSecret": "",
  "accountId": "",
  "accountEmail": "",
  "accountName": "",
  "projectId": "",
  "clientVersion": "1.15.8",
  "maxConcurrentGenerations": 2,
  "quotaPollIntervalSeconds": 300,
  "quotaHistoryMaxSamples": 10000,
  "quotaHistoryPath": "/home/your-user/.local/share/antigravity-proxy/usage.jsonl",
  "dailyEndpoint": "https://daily-cloudcode-pa.googleapis.com",
  "prodEndpoint": "https://cloudcode-pa.googleapis.com"
}
```

Login writes `accountId`, `accountEmail`, and `accountName` together with the credential set. Keep these managed fields paired with their tokens; runtime replacement updates them atomically and never persists listener defaults or environment overrides. Token environment variables, including explicit empty values, clear saved profile attribution in the effective configuration. Remove token overrides to use `/config/login`.

`quotaHistoryPath` is the journal base name. The runtime derives an owner-only journal with a stable hashed Google-account namespace; relogin to the same account and restart retain that account's observations. Unattributed legacy journals stay untouched. Credentials without identity are resolved read-only; unresolved credential sets do not fetch quota or expose another account's journal.

`/config/login` requires a configured `API_KEY` even on loopback, and its credential configuration directory must be writable. Static read-only configuration mounts remain supported for preconfigured deployments, but cannot persist runtime login.

---

## Environment Variables

Every setting can be specified or overridden using environment variables:

| Environment Variable | Description | Default |
|---|---|---|
| `HOST` | Network interface to bind to (`127.0.0.1`, `0.0.0.0`, etc.) | `127.0.0.1` (Binary) / `0.0.0.0` (Docker) |
| `PORT` | Listening port for the HTTP server | `8080` |
| `API_KEY` | Proxy client authentication key. **Required** if `HOST` is non-loopback. | Unset |
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
