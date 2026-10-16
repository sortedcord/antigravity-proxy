# Versioning and executable identity

The current version baseline is **v0.1.0**.
`internal/buildinfo/VERSION` is the source of truth for the baseline. Go module
language requirements and the upstream Antigravity client compatibility version are
independent of this application version.

## Release policy

Use SemVer `MAJOR.MINOR.PATCH`, with a `v` prefix on Git tags:

- During the v0 phase, major feature updates increment MINOR and reset PATCH:
  `v0.1.0` → `v0.2.0`.
- Fixes and small updates increment PATCH within the current feature series:
  `v0.1.0` → `v0.1.1`. Do not reset the minor number to zero for patch releases.
- v0 is a stabilization phase; compatibility is not yet promised. Document
  breaking changes explicitly in release notes, preferably in a feature release.
- Only the maintainer decides when the proxy setup is solid enough for `v1.0.0`.
  There is no automatic graduation to v1.
- From v1 onward: breaking compatibility increments MAJOR, backward-compatible
  features increment MINOR, and backward-compatible fixes increment PATCH.
- Prereleases may use suffixes such as `v0.2.0-rc.1`. Untagged working builds
  use `<baseline>-dev`; they must not masquerade as stable releases.

Update the baseline in the same reviewed change as a version bump. Create the
matching tag only after approval/merge. Adding version identification does not
itself publish a release or create a tag.

## Automated releases with Release Please

Releases and Git tags are managed automatically by [Release Please](https://github.com/googleapis/release-please):

1. **Write Conventional Commits**: When opening pull requests or merging commits into `master`,
   use Conventional Commit prefixes:
   - `feat:` triggers a minor release bump in the v0 phase (`v0.1.0` → `v0.2.0`).
   - `fix:` triggers a patch release bump in the v0 series (`v0.1.0` → `v0.1.1`).
   - Other types (`docs:`, `perf:`, `chore:`, `refactor:`) update release notes without a version bump.
2. **Release PR**: On pushes to `master`, Release Please updates or creates a dedicated
   Release PR that updates `CHANGELOG.md` and `internal/buildinfo/VERSION`.
3. **Triggering a Release**: Whenever the maintainer is ready to release, simply review and
   merge the Release PR into `master`. Merging it will:
   - Create the corresponding Git tag (e.g. `v0.1.1`).
   - Create the GitHub Release with formatted changelog notes.
   - Run GoReleaser to compile static binaries (Linux, macOS, Windows) and upload checksummed archives.
   - Build and publish multi-arch Docker container images to GHCR.
4. **Manual Fallback**: Pushing a manual tag (`git tag v0.1.1 && git push origin v0.1.1`) triggers
   the release workflow.

## Inspecting a build

```sh
antigravity-proxy version
# antigravity-proxy v0.1.0 (commit=<commit>, date=<RFC3339 timestamp>)
```

The output is one line on stdout; errors go to stderr. No flags or positional
arguments are accepted after `version`. It does not load configuration, contact
upstream, read credentials, acquire locks, or initialize history. Startup's
`listening` log includes the same version, commit, and date fields.

## Building

```sh
./scripts/build.sh                         # bin/antigravity-proxy; v0.1.0-dev
VERSION=v0.1.0 ./scripts/build.sh          # release identity, no publication
./bin/antigravity-proxy version
```

The script injects the Git commit and commit timestamp (not wall-clock build time,
so identical source builds are reproducible). A modified checkout's commit is
marked `-dirty`. `VERSION`, `COMMIT`, and `BUILD_DATE` may be supplied explicitly
by release tooling, including tooling building from a source archive without Git.
Metadata must contain no whitespace; `BUILD_DATE` should use RFC3339.

Plain `go build` uses the embedded baseline plus `-dev` and Go's VCS metadata
where available. `go install ...@v0.1.0` can use Go's main-module version as a
fallback. Missing commit/date metadata is reported as `unknown`, never empty.

Linker fields for release tooling:

```text
-X antigravity-proxy/internal/buildinfo.Version=v0.1.0
-X antigravity-proxy/internal/buildinfo.Commit=<commit>
-X antigravity-proxy/internal/buildinfo.Date=<RFC3339 timestamp>
```

Explicit linker metadata takes precedence over Go build information. Do not put
keys, tokens, or other runtime configuration in these fields.

Docker accepts equivalent `VERSION`, `COMMIT`, and `BUILD_DATE` build arguments.
Local Docker builds default to the embedded development baseline with unknown
commit/date because Git is intentionally excluded from the build context. The
publishing workflow injects the checked-out commit and commit timestamp, and uses
the tag for version builds or the baseline plus `-dev` for branch/PR builds.
