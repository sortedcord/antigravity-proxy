# Architecture & Internal Design

Antigravity Proxy functions as a protocol translator between the standard Google Gemini `/v1beta` API and Google Cloud Code's internal Antigravity service.

```
+------------------------------------+
| Client / Gateway                   |
| (Bifrost, Cline, Aider, OpenCode)  |
+-----------------+------------------+
                  |  Gemini /v1beta API (HTTP + SSE)
                  v
+-----------------+------------------+
| Antigravity Proxy                  |
|                                    |
| - Gemini Route Adapter             |
| - Local API Key Auth               |
| - In-Memory Token & Catalog Cache  |
| - Quota Polling & History Storage  |
+-----------------+------------------+
                  |  Internal Cloud Code Protocol (Bearer OAuth)
                  v
+-----------------+------------------+
| Google Antigravity Upstream        |
| (daily- / cloudcode-pa.googleapis) |
+------------------------------------+
```

---

## 1. Upstream Protocol Adaptation

Antigravity models run on Google's private Cloud Code infrastructure. When a client makes a standard Gemini request, the proxy performs the following adaptations:

- **Envelope Wrapping**:
  Requests are wrapped into Cloud Code's internal schema containing `project`, `model`, `request`, `userAgent`, and `requestType: "agent"`.
- **Response Unwrapping**:
  The response is extracted from Cloud Code's envelope and streamed or returned to the client as pristine Gemini JSON.
- **Structured Output Compatibility**:
  Renames `generationConfig.responseJsonSchema` to `generationConfig.responseSchema` to match Cloud Code's expected field naming.
- **SSE Streaming**:
  Streams Server-Sent Events incrementally with automatic error translation and clean EOF handling.

---

## 2. Authentication & Client Credentials

Antigravity access requires authenticating against Google with the official consumer OAuth client identity.

### Consumer vs. GCP Client Branch

Google distinguishes between two OAuth client configurations:
1. **Cloud Code / GCP Client**: Used for Google Cloud Platform integrations. Google rejects individual consumer Antigravity requests under this client (`GOOGLE_TOS_NOT_SUPPORTED_BY_CLIENT`).
2. **Consumer Client**: Used for official Antigravity IDE and consumer subscriptions.

The proxy's `internal/oauth` package uses the official consumer OAuth client credentials to authenticate with Google OAuth. Users can also override these via the `ANTIGRAVITY_OAUTH_CLIENT_ID` and `ANTIGRAVITY_OAUTH_CLIENT_SECRET` environment variables.
### Token Flow & Storage

1. **Authorization**: `antigravity-proxy login` opens Google's OAuth consent screen with PKCE (`S256`).
2. **Callback**: A temporary local server receives the authorization code on loopback (`http://127.0.0.1:51121/oauth-callback`).
3. **Storage**: Tokens and the paired OAuth client credentials are saved to `~/.config/antigravity-proxy/config.json` with `0600` permissions.
4. **Serving**: At runtime, access tokens are refreshed automatically in memory before expiration. Tokens are never written back to disk during normal serving.

---

## 3. Project Discovery & Onboarding

If `ANTIGRAVITY_PROJECT_ID` is not explicitly configured:
1. The proxy calls `/v1internal:loadCodeAssist` on startup to discover the user's provisioned Antigravity cloud companion project.
2. If no project exists yet, it triggers `/v1internal:onboardUser` with the allowed subscription tier.
3. The discovered project ID is cached in memory and automatically attached to subsequent model generation requests.

---

## 4. Quota Polling & Durable History

Antigravity operates on periodic 5-hour and weekly quota buckets for Gemini and Claude/GPT models.

- **Background Polling**:
  A single background goroutine polls `/v1internal:retrieveUserQuotaSummary` every 5 minutes (configurable).
- **Single-Writer Exclusive Lock**:
  History is stored in an append-only JSONL file (`usage.jsonl`). An exclusive file lock (`usage.jsonl.lock`) ensures only one proxy process writes to the file at any time.
- **Cross-Platform Locking**:
  Uses POSIX file locking (`fcntl`) on Unix systems and `LockFileEx` on Windows.
- **Atomic Compaction & Retention**:
  When the file reaches the maximum sample count (default: 10,000 observations), older entries are pruned via an atomic temporary file replacement to prevent uncontrolled disk growth.

---

## 5. Codebase Structure

```text
cmd/antigravity-proxy/
  main.go                     # CLI entrypoint (serve, login) and signal management
internal/
  config/                     # Configuration schema, validation, and file persistence
  oauth/                      # OAuth client credentials, PKCE login flow, token refresh
  proxy/                      # Reverse proxy, Gemini HTTP handler, SSE streaming
  quota/                      # Quota fetcher and upstream response parsing
  status/                     # Background quota polling, durable storage, query engine
```
