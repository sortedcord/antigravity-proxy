# Quota Redesign: Upstream-Driven Buckets and Memory-Only Status

Status: implementation proposal; no implementation changes made by this document.

## 1. Goal and scope

Represent the quota constraints Google actually reports, rather than forcing every account into two named pools with two mandatory windows. Keep quota inspection useful without making the proxy responsible for analytics, usage history, or quota persistence.

This is a v0 breaking change. Do not build API compatibility adapters, history migrations, or legacy snapshot readers.

The work has two dependent milestones:

1. Replace fixed quota slots with a flat bucket collection and remove synthesized windows.
2. Remove quota history and storage after the multi-account feature, retaining only a latest-observation cache in memory.

Implement both milestones on the same feature branch before shipping the final design. Prefer removing history before integrating the new model, rather than redesigning historical filtering/pagination that will immediately be deleted. If the team requires separate delivery dates, hold the final schema change until history removal is ready; do not invest in a new history schema.

### Approved constraints

- Breaking API/config changes are acceptable.
- No backwards compatibility is required.
- Quota history and storage are to be removed.
- Multi-account data/admin authentication boundaries must remain unchanged.

### Proposed defaults

These are design recommendations, not additional requirements already confirmed by the team:

- Keep periodic account-scoped polling, with the existing 300-second default.
- Keep exactly one latest successful snapshot per account in memory.
- Keep quota reads cache-only; they do not synchronously call upstream.
- Keep existing current-quota route names, but change their response schema.
- Use a flat `buckets` array, not a generic tag bag or a nested fixed `pools` object.

Removing storage does not inherently require removing polling. If the team instead wants on-demand quota passthrough, decide that before the memory-cache phase; that alternative needs explicit request coalescing, cache expiry, and aggregate request concurrency limits.

## 2. Why the current design must change

Current implementation assumptions:

- `internal/quota/quota.go`: `Pools` has `Gemini` and `ThirdParty`; `Pool` has `FiveHour` and `Weekly`.
- `internal/quota/snapshot.go`: `Buckets()` always returns four canonical slots and validation expects all of them.
- Parsing only accepts the four known bucket IDs; unfamiliar buckets are silently ignored.
- `internal/status/history.go`: filtering/pagination assumes four slots and uses positional arithmetic.
- `internal/status/collector.go` and `storage*.go`: file locks, append/sync, retention, recovery, and history indices are part of status collection.
- `internal/proxy/runtime.go`: account preparation depends on history paths and storage initialization.

Live investigation observed:

- The inspected Google AI Pro account reports `gemini-5h`, `gemini-weekly`, `3p-5h`, and `3p-weekly`.
- The inspected Google AI Plus account reports only `gemini-weekly` and `3p-weekly`.
- Both upstream origins returned valid responses with that distinction.
- The Plus account can successfully perform inference.

These observations describe the inspected accounts, not a permanent tier-to-quota contract.

Commit `fcd18a5` copies weekly values into an absent five-hour slot. Remove that behavior. A weekly quota with a weekly reset is not an upstream five-hour quota. Absence does not prove unlimited access, and the proxy must not invent constraints or percentages.

## 3. Target data contract

### Snapshot

A snapshot consists of:

- `observed_at`: UTC time of a successful upstream observation.
- `source`: `retrieveUserQuotaSummary`.
- `buckets`: an initialized array of the reported buckets; never JSON `null`.

Example for a weekly-only response:

```json
{
  "observed_at": "2026-10-13T11:17:11Z",
  "source": "retrieveUserQuotaSummary",
  "buckets": [
    {
      "id": "gemini-weekly",
      "pool_display_name": "Gemini Models",
      "window": "weekly",
      "display_name": "Weekly Limit Remaining",
      "status": "available",
      "remaining_fraction": 0.9983656,
      "reset_at": "2026-10-20T11:13:52Z"
    },
    {
      "id": "3p-weekly",
      "pool_display_name": "Claude and GPT models",
      "window": "weekly",
      "display_name": "Weekly Limit Remaining",
      "status": "available",
      "remaining_fraction": 1,
      "reset_at": "2026-10-20T11:17:11Z"
    }
  ]
}
```

### Bucket fields

| Field | Contract |
| --- | --- |
| `id` | Required upstream bucket ID, preserved verbatim. Identity within a snapshot. |
| `pool_display_name` | Optional upstream group display name (`displayName` from group). Presentation only. |
| `window` | Optional upstream window label, e.g. `5h`, `weekly`, or a future value. Open string, not a duration inferred from a timestamp. |
| `display_name` | Optional upstream bucket display name (`displayName` from bucket). |
| `status` | `available`, `disabled`, or `unavailable`. This describes the reported bucket, not inference availability. |
| `remaining_fraction` | Optional number in `[0, 1]`; explicit zero is meaningful. |
| `remaining_amount` | Optional upstream int64 string. Preserve precision; do not invent units. |
| `reset_at` | Optional parsed upstream reset timestamp. |
| `unavailable_reason` | Present for disabled/unusable reported buckets when appropriate. Safe normalized reason, not a raw provider error body. |

Use optional pointer fields for quota values. Omit missing optional values in JSON. Do not store `remaining_percent` and `used_percent`: clients can calculate `fraction * 100` and `(1 - fraction) * 100` for display. Do not retain a redundant `disabled` boolean alongside `status`.

Use typed Go structs and slices. No `map[string]any` for the domain model and no `tag1`/`tag2` fields. A map indexed by bucket ID may be used locally for deduplication.

### Pool classification boundary

The observed upstream groups have display names but do not provide a stable group ID. Therefore:

- Preserve group display names independently from normalized labels.
- Isolate the small mapping for currently recognized bucket IDs to `gemini`/`third_party` in the upstream adapter.
- When the explicit upstream `window` is absent, that same adapter may supply a known window for a recognized bucket ID. An unfamiliar ID has no guessed window.
- Unknown IDs remain in the collection with their explicit upstream metadata, even if `pool` is absent.
- Never split arbitrary bucket IDs on hyphens to infer meaning, derive identity from localized display names, or infer windows from reset distances.
- Adding a classification alias must not require changing snapshot structs, validation, collection, or routes.

This preserves helpful familiar labels without making four specific buckets the schema.

### Ordering and identity

- Deduplicate by upstream bucket ID, not array position or `(pool, window)`.
- Distinct buckets may share a pool and window.
- Sort output by bucket ID for deterministic serialization.
- Do not attach user/account identity to reusable quota structs; account ownership belongs to the runtime and aggregate API envelope.

## 4. Parser and validation behavior

Refactor parsing into a small upstream adapter plus generic bucket validation.

1. Decode the bounded response and supported `response`/`summary` wrappers.
2. Traverse supported `groups` and canonical `buckets` arrays, retaining group display metadata.
3. Require a valid non-empty bucket ID for each reported bucket. Missing/malformed IDs fail the observation rather than silently losing a quota constraint.
4. Preserve unfamiliar IDs and window labels.
5. Normalize recognized IDs only at the classification boundary.
6. Parse optional quota values, timestamps, and disabled status.
7. Deduplicate by ID; accept semantically equivalent repeated buckets and reject conflicting duplicates.
8. Sort and return the reported collection. Never create entries for absent buckets.

Envelope policy:

- Recognized empty arrays are a successful observation with `buckets: []`.
- A syntactically valid object with no recognized quota container is an unsupported response shape, not a successful empty quota observation.
- Invalid array types, malformed JSON, excess nesting, or conflicting duplicates fail the observation.
- Unknown extra metadata fields do not fail parsing.

Value policy:

- An explicitly disabled bucket is `disabled`; it must not advertise usable remaining quota values.
- A reported bucket with invalid quota fields is `unavailable`, with a specific reason; do not fabricate values or clamp invalid fractions.
- A bucket containing only a valid amount may be `available` without a fraction.
- A bucket lacking both valid fraction and amount is `unavailable` with `quota_not_reported`.
- Preserve valid optional metadata such as reset time where useful, but do not mark invalid measurements available.
- Keep validation of fractions, int64 amounts, timestamps, identity, and statuses generic.

Continue existing HTTP/body limits, timeouts, safe transport errors, and shared account token/project transport. Do not add per-model quota substitutions, tier-specific parsing, or new upstream requests during generation.

## 5. Memory-only quota status

### Responsibilities

Keep `internal/quota` responsible for upstream fetching and quota interpretation. Keep `internal/status` responsible only for latest-value caching, polling lifecycle, staleness, and status responses. The proxy runtime continues to own account lifecycle and authentication.

Replace the storage-backed collector with a small account-scoped cache/worker owning:

- One latest successful `quota.Snapshot`, plus a flag indicating whether one has been observed.
- Poll metadata: interval, last attempt, last success, and credential-safe last error.
- Mutex-protected state, cancellation, and worker completion.
- A coalescing trigger channel; never overlapping polls for the same account.

No samples slice, history index, file path, lock file, retention count, compaction, or durability state.

### Read and failure semantics

- A status read returns an independent copy of cached buckets and their pointer fields.
- Do not hold cache locks across network calls.
- A successful empty observation replaces the previous snapshot with an empty array.
- An upstream/parse failure preserves the last successful snapshot and updates poll metadata.
- Mark retained data stale on an error or after the documented age threshold; retain the existing two-poll-interval age threshold initially.
- Before the first successful observation, account quota routes return HTTP 503 with poll metadata and a safe explanation.
- Stale retained observations return HTTP 200 with `stale: true`; clients must not mistake them for fresh data.
- Restart clears all quota observations. There is no restoration from disk.
- Poll success means fetched, validated, and published in memory—not durably stored.

### Account lifecycle and isolation

- Begin polling only after the account runtime is published/active, avoiding an immediate fetch against an unpublished candidate.
- Keep reads independent of whether the selected account is currently performing inference.
- Metadata-only changes and key changes preserve the same cache/worker.
- Re-login to the same verified Google identity may preserve its last snapshot, marked stale until refreshed; invalidate any old in-flight publication.
- A different identity, unresolved-to-resolved identity transition, or account replacement gets a fresh cache.
- Use runtime identity/generation checks to prevent old workers/results publishing into replacement accounts. Do not retain filesystem namespace hashing solely for quota storage.
- Account deletion cancels and joins its worker. Avoid joining workers while holding runtime locks they need to finish.
- Proposed default: disabled accounts do not poll; retain any cached result as stale and trigger a fresh poll on re-enable.
- Shutdown cancels and joins workers without waiting for unrelated inference or token-refresh work.

Review identity/generation helpers before deleting them: some currently also prevent cross-account publication, independently of their former history-path use.

## 6. HTTP surface

Keep authentication and account scoping unchanged.

| Route | Target behavior |
| --- | --- |
| `GET /status/limit` | Caller-account snapshot with `buckets`, `polling`, and `stale`. |
| `GET /admin/accounts/{id}/quota` | Same quota view for the specified account. |
| `GET /admin/quota` | Sorted account array; each item has `id`, `buckets`, `observed_at` when available, `polling`, and `stale`. Include an explicit pending/unavailable state when no successful observation exists. |
| `GET /admin/accounts` | Account metadata plus the new cached quota snapshot where available. |
| `GET /status/account` and admin account detail | Preserve account/subscription behavior; no quota storage dependency. |
| `GET /status/usage` | Remove route and implementation; requests return 404. |
| `GET /admin/accounts/{id}/usage` | Remove route and implementation; requests return 404. |

Aggregate quota reads must not fetch upstream or fail the entire response because one account has never produced a snapshot. A successful empty collection must be distinguishable from a missing observation.

No backwards-compatible `pools` field, five-hour aliases, usage endpoints, historical query parameters, or legacy response adapters.

Quota inspection remains observational. Do not reject inference locally based on cached quota; authoritative upstream HTTP 429 responses still drive gateway failover.

## 7. Configuration and deployment cleanup

Remove quota-history settings and validation:

- `QuotaHistoryPath` / JSON `quotaHistoryPath`.
- `QuotaHistoryMaxSamples` / JSON `quotaHistoryMaxSamples`.
- `DefaultQuotaHistoryMaxSamples`.
- `ANTIGRAVITY_QUOTA_HISTORY_PATH`.
- `ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES`.

Keep the poll interval setting and safe duration validation if periodic polling is retained.

Remove history mounts/directories from Docker and Compose examples. A writable configuration directory is still required for management login/account/key changes; removing quota storage does not make the entire application configuration read-only.

Because config decoding is strict, old history JSON fields need manual removal before startup. Document that explicitly; do not add a migration solely to retain compatibility. Document removal of old environment variables as well.

Do not delete existing history files automatically. They can be removed manually after verification, but the new runtime must neither read nor write them.

Keep `internal/atomicfile` and configuration durability handling: account credentials and key configuration still require safe persistence.

## 8. Implementation sequence and file-level work

### Phase 1 — Remove history-dependent status behavior

- Rewrite `internal/status/collector.go` as latest-only memory state plus polling, or replace it with a clearly named cache type and update callers.
- Simplify `internal/status/service.go`: remove `Prepare`, storage options, `AccountHistoryPath`, usage parsing, and HTTP history handling.
- Delete `internal/status/history.go`, `storage.go`, and platform-specific `storage*.go` implementations.
- Delete storage-only tests; retain/rewrite concurrency, staleness, lifecycle, cancellation, and failure-recovery tests.
- Remove usage routes from `internal/proxy/routes.go`, `api.go`, and `admin.go`.
- Remove history configuration/environment/defaults and update affected config tests.
- Simplify runtime account preparation/activation/cleanup to use memory-only status. Preserve credential commit/publish guarantees.
- Finish this phase with working latest-status behavior before changing the quota model.

### Phase 2 — Replace the domain model and adapter

- `internal/quota/quota.go`: replace `Pools`/fixed `Pool`/mandatory windows with `Snapshot.Buckets []Bucket` and generic bucket fields.
- `internal/quota/snapshot.go`: replace `[4]Bucket`, fixed identity checks, and pointer-to-slot traversal with slice validation and deep cloning.
- Remove the five-hour synthesis added in `fcd18a5` and its test; add tests proving no bucket is synthesized.
- Isolate known-ID classification from generic parsing and ensure unknown IDs are retained.
- Update `fetch.go`, `transport.go`, and fetch tests as needed without changing the credential-safe transport boundary.

### Phase 3 — Integrate response schemas

- Update data/admin quota handlers and account summary serialization.
- Share one cache-read view across routes so staleness/pending semantics cannot drift.
- Sort aggregate accounts and bucket output deterministically.
- Rewrite proxy/status tests using variable-size snapshots instead of `.Pools.Gemini.FiveHour` assertions.
- Verify that no account API key is accepted for admin access and no other account's snapshot leaks through data routes.

### Phase 4 — Documentation and local workflow

- Update `docs/api.md`, `docs/configuration.md`, `docs/deployment.md`, `docs/architecture.md`, and `README.md` where applicable.
- Remove claims about historical usage, persisted snapshots, required history directories, retention, and mandatory five-hour/weekly windows.
- Update `docs/plans/multi-account.md` to describe current-quota inspection rather than historical usage routes.
- Include examples for Pro-like four-bucket, Plus-like two-bucket, unknown-bucket, empty, and stale responses.
- The local Python management/testing helper remains untracked and must not be included in commits. If used for verification, adapt it locally only.

Each phase should be a focused, buildable commit. Do not combine unrelated runtime refactoring or CLI fixes with the quota redesign.

## 9. Verification plan

### Parser/model tests

- Four independently reported Pro-like buckets remain four with independent values/reset times.
- Two reported weekly-only buckets remain exactly two; no `5h` entry appears.
- Future daily/monthly/unknown IDs survive parsing without a new struct field.
- Missing pool classification and missing optional window do not discard unfamiliar buckets.
- Two distinct IDs with the same pool/window remain distinct.
- Explicit zero and one remain distinguishable from missing measurements.
- Amount-only values retain exact int64 strings and do not produce fractions.
- Reported disabled/malformed/missing-value buckets retain correct status/reasons.
- Known empty containers succeed with an empty array; unsupported/malformed envelopes fail.
- Wrappers, nesting limit, equivalent duplicates, conflicting duplicates, deterministic sorting, and deep-copy isolation are tested.

### Cache/lifecycle tests

- Initial pending state, successful empty state, immediate first poll, periodic refresh, and trigger coalescing.
- Failed refresh retains last good data and marks it stale; subsequent success clears the error.
- Reads/polls/close are race-free and snapshots do not alias cache memory.
- No overlapping polls per account; cancellation interrupts an in-flight fetch.
- Metadata/key changes preserve cached data; identity replacement prevents old data/result publication.
- Disable/re-enable/delete/shutdown obey the chosen worker policy.
- No quota directory/file/lock creation, no loading of existing JSONL files, and no restored snapshot after restart.
- Removed usage routes return 404.

### Live verification with local credentials

- Obtain credentials only from the local private config or existing authenticated API. Never print or commit OAuth tokens, refresh tokens, client secrets, account keys, or the admin key.
- Fetch raw summaries for the two available test accounts using the existing upstream request headers.
- Compare reported bucket ID sets, window labels, fractions, and reset timestamps to the new parser output; assert equality of actual reported constraints, not a tier-based expected count.
- Verify unknown/empty scenarios with sanitized fixtures; do not generate excessive upstream requests to obtain them.
- Exercise the new account and aggregate HTTP schemas using an isolated test runtime or an explicitly approved local restart. Do not start a second production runtime sharing the writable account config.
- If creating temporary keys for endpoint tests, revoke them in cleanup and surface any cleanup failure; prefer isolated runtime keys where possible.
- Ensure the observed Pro account remains independent per window and the observed Plus account contains only its two weekly buckets, if that is still what Google reports at test time.
- Do not require inference for parser verification. If a smoke generation is useful, send only a short prompt and avoid large output.
- Leave the current live server/config untouched unless explicitly approved. Temporary verification tools and credential-bearing files must be cleaned up and excluded from commits.

Run:

```sh
gofmt -w <changed-go-files>
go vet ./...
go test -count=1 ./...
go test -count=1 -race ./...
go build ./cmd/antigravity-proxy
git diff --check
```

Check affected supported platform builds after removing platform-specific storage code. Inspect staged paths/content before committing; do not stage the local Python helper or runtime artifacts.

## 10. Completion criteria

- [ ] No mandatory Gemini/third-party or five-hour/weekly slot layout remains in the domain model.
- [ ] Parser preserves unfamiliar reported buckets and never manufactures absent buckets.
- [ ] Weekly-only upstream responses have only weekly entries in all quota APIs.
- [ ] Percentages are presentation-derived; zero and missing values remain distinct.
- [ ] Every account has at most one cached quota observation, with bounded worker state.
- [ ] No quota history files, storage locks, recovery, retention, pagination, or usage endpoints remain.
- [ ] History settings and deployment mounts are removed; manual config cleanup is documented.
- [ ] Credential/config persistence, API-key routing, account isolation, and upstream 429 behavior are unchanged.
- [ ] Poll failure, identity replacement, restart, and cache cloning are verified under the race detector.
- [ ] Live raw-response comparison succeeds without leaking secrets or committing verification scripts.

## 11. Explicit non-goals

- Usage accounting, token-cost tracking, dashboards, or historical analytics.
- Per-model quota estimates or converting model quotas into shared-pool quotas.
- Locally deciding that an account can/cannot perform inference based on a cached fraction.
- Inferring permanent quota policies from subscription names.
- API/history backwards compatibility, legacy-history import, or automatic file deletion.
- Changing Bifrost rotation behavior or introducing proxy-side account rotation.
