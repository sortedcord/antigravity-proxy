# Multi-Account Architecture & Implementation Plan

## Overview
Antigravity Proxy supports multiple Google accounts within a single server instance, allowing AI gateways like Bifrost to route requests to distinct Google accounts with per-account quota tracking, metrics isolation, and automatic 429 failover.

The architecture strictly separates the **Data Plane** (`/v1beta/*`, `/models`, `/status/*`) from the **Admin Plane** (`/admin/*`).

---

## Architectural Decisions

### 1. Data vs. Admin Plane Authentication
- **Data Plane**: Authenticated using account-scoped API keys (`agp_...`). The key maps in constant time to an active account runtime. Supports headers (`x-goog-api-key`, `x-api-key`, `Authorization: Bearer`) and query parameter (`?key=...`).
- **Admin Plane**: Authenticated strictly using `ADMIN_API_KEY` via headers (`x-goog-api-key`, `x-api-key`, `Authorization: Bearer`). Query parameters (`?key=...`) are explicitly rejected to prevent administrative secrets from leaking in URLs or access logs. If `ADMIN_API_KEY` is not configured, admin endpoints return `503 Service Unavailable`. Legacy data `API_KEY` values never grant administrative access.

### 2. Credential Storage & Key Hashing
- API keys are stored only as SHA-256 hashes (`sha256:<hex>`).
- Plaintext secrets (`agp_` prefix followed by 256 bits of base62 entropy) are revealed only once upon generation.
- An immutable in-memory registry (`apikey.Registry`) provides constant-time lookup from raw secret to account and key ID bindings.

### 3. Account Identity & Routing Rules
- Accounts have unique slugs (`a-z0-9-`).
- When logging in via OAuth, the proxy matches an existing account by Google ID (`google:<id>`), or derives an ID slug from the email local-part.
- Attempting to link a Google account already assigned to another proxy account returns `409 Conflict`.
- Re-linking an existing account to a different Google account requires `replace: true`, otherwise returning `409 Conflict`.
- Accounts defined or overridden by environment variables (`ANTIGRAVITY_ACCESS_TOKEN`, `ANTIGRAVITY_REFRESH_TOKEN`) are immutable at runtime and return `409 Conflict` if targeted by administrative mutations.

### 4. Runtime Registry & Cache Preservation
- An atomic snapshot pointer (`atomic.Pointer[runtimeRegistry]`) holds the active accounts, quotas, and key bindings.
- When configuration updates on disk, the runtime re-reads disk state and merges environment layers (`API_KEY` and token overrides).
- Unchanged accounts retain their existing `*accountRuntime`, proxy instances, cached tokens, and status services, avoiding redundant upstream discovery or token invalidation.
- Revoked keys are pruned from `lastUsed` tracking.

---

## Phased Implementation Summary

### Phase 1: Config Model & Migration (`internal/config`)
- `Account` and `AccountKey` structs.
- In-memory migration from legacy flat configuration to `default` account on load.
- Safe, owner-only disk reader (`config.ReadDiskAccounts`).
- Atomic read-modify-write persistence (`config.UpdateAccounts` and `config.UpdateAdminKey`).
- Explicit credential targeting in `UpdateCredentials`.

### Phase 2: Key Primitives (`internal/apikey`)
- `Generate()` (`agp_` prefix with 256 bits of base62 entropy).
- `GenerateID()` (`key_...`).
- `Hash()` and `HashBytes()` (`sha256:` prefix).
- Header and query extraction with `allowQuery` security flag.
- Immutable `Registry` with `Lookup()`, `HasKeyID()`, and constant-time matching.

### Phase 3: Multi-Account Runtime & Routing (`internal/proxy`)
- Multi-account registry snapshot (`atomic.Pointer[runtimeRegistry]`).
- Request dispatch mapping incoming API keys to account proxies.
- Removal of `/config/login`.
- Scoped status endpoints (`/status/limit`, `/status/usage`, `/status/account`) reflecting caller's account.
- Public `/health` reporting overall proxy readiness without leaking account topology.
- Account-tagged access logging.

### Phase 4: Login Sessions (`internal/oauth`)
- `SessionManager` supporting session IDs (`ls_...`), target account names, `replace` flag, and optional `issue_key`.
- Session retention map allowing status inspection across multiple concurrent sessions.
- One-time key issuance for loopback callbacks and manual completion.

### Phase 5: Admin API (`internal/proxy/admin.go`)
- `/admin/accounts`: List, get, patch, delete accounts.
- `/admin/login-sessions`: Create, inspect status, and complete login sessions.
- `/admin/accounts/{id}/keys`: List, create, and revoke account API keys.
- `/admin/accounts/{id}/quota` & `/admin/accounts/{id}/usage`: Account quota inspection.
- `/admin/quota`: Overview of all accounts with poll-interval staleness checks and sorted JSON array output.

### Phase 6: CLI (`cmd/antigravity-proxy`)
- `login [--account ID] [--replace] [--issue-key LABEL]`
- `accounts <list|enable|disable|remove> [ID]`
- `keys <list|create|revoke> <ACCOUNT_ID> [KEY_ID] [--label LABEL]`
- `admin-key rotate`

### Phase 7: Documentation
- `docs/api.md`: Data vs. Admin endpoints reference, authentication models, session lifecycles.
- `docs/configuration.md`: Multi-account JSON schema, environment variables, automatic migration.
- `docs/deployment.md`: Bifrost multi-key failover and per-account metrics guide.
- `README.md`: Multi-account features and administration quickstart.
