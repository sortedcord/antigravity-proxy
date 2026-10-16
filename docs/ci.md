# Continuous Integration and Release Pipeline

This document describes the unified CI/CD architecture, validation gates, release management, and local developer verification for `antigravity-proxy`.

## Pipeline architecture

The pipeline consists of modular, reusable GitHub Actions workflows:

| Workflow | Path | Trigger | Purpose |
| :--- | :--- | :--- | :--- |
| **CI** | `.github/workflows/ci.yml` | Pull requests and pushes | Entrypoint orchestrating all validation suites; provides the required gate `ci-required` |
| **Validation** | `.github/workflows/validate.yml` | `workflow_call` | Go matrix testing, cross-platform builds, container smoke tests, snapshot packaging |
| **Code Quality** | `.github/workflows/code-quality.yml` | `workflow_call` | Formatting, strict `golangci-lint`, test coverage baseline, script and doc validation |
| **Security** | `.github/workflows/security.yml` | `workflow_call`, schedule | Dependency vulnerability scanning (`govulncheck`), secret detection, workflow linting |
| **Container Release** | `.github/workflows/docker.yml` | Push to default branch / tags, `workflow_dispatch` | Builds and publishes hardened multi-arch OCI images to GHCR |
| **Release Management** | `.github/workflows/release-please.yml` | Push to default branch | Automates changelog generation, SemVer version bumps, and release PRs |
| **Release Publisher** | `.github/workflows/release.yml` | Push of `v*` tags | Sole publisher of checksummed release binaries via GoReleaser and release containers |
| **Maintenance** | `.github/workflows/maintenance.yml` | Weekly schedule, `workflow_dispatch` | Extended mutation fuzzing, concurrency stress tests, benchmark tracking |
| **PR Title Policy** | `.github/workflows/pr-policy.yml` | Pull request metadata events | Validates Conventional Commits title format |
| **Dependabot** | `.github/dependabot.yml` | Weekly schedule | Automated updates for GitHub Actions, Docker base images, and Go modules |

## Execution model

```text
Pull request / branch push
  └─ CI (.github/workflows/ci.yml)
      ├─ Code Quality (formatting, linting, coverage baseline >= 70%, scripts, docs)
      ├─ Security (secret scan, govulncheck)
      ├─ Validation
      │   ├─ Go compatibility matrix (1.22.x, 1.27.2, stable)
      │   ├─ Native build & executable identity verification
      │   ├─ Container build & hardened runtime smoke test
      │   ├─ Release archive snapshot packaging
      │   ├─ Cross-platform validation (macOS, Windows)
      │   └─ Project-specific routing and configuration tests
      └─ ci-required (aggregate gate for branch protection)

Default branch push (master)
  ├─ CI validation pipeline
  ├─ Development container publication (tags: master, sha-<commit>)
  └─ Release Please (creates/updates release PR)

Release PR merge (or manual v* tag push)
  └─ Release workflow (.github/workflows/release.yml)
      ├─ Validates exact tag and commit
      ├─ Compiles static binaries for Linux, macOS, Windows via GoReleaser
      ├─ Generates checksums.txt and uploads GitHub Release assets
      └─ Builds and publishes versioned multi-arch container to GHCR (tags: X.Y.Z, X.Y, X, latest)
```

## Container image tagging rules

- **Branch pushes**: published with `<branch-name>` and `sha-<full-sha>` (never `latest`).
- **Stable releases (`vX.Y.Z`)**: published with `X.Y.Z`, `X.Y`, `X`, and `latest`.
- **Prereleases (`vX.Y.Z-rc.1`)**: published with `X.Y.Z-rc.1` and `sha-<full-sha>` (never `latest` or major/minor moving aliases).
- **Pull requests**: images built and smoke-tested without publishing or registry write credentials.

## Toolchain policy

- **Module language compatibility**: Go `1.22` (specified in `go.mod`).
- **Production compiler**: Go `1.27.2` (used in `Dockerfile` and release builds).
- **Forward compatibility**: Go `stable` tested in matrix.
- **Linters**: pinned `golangci-lint` v2.14.0.

## Local developer verification commands

Run these scripts before opening or merging a pull request:

```sh
# 1. Format and lint checks
./scripts/ci/check-quality.sh

# 2. Syntax check for shell and Python scripts
./scripts/ci/check-scripts.sh

# 3. Documentation and link validation
./scripts/ci/check-docs.sh

# 4. Secret scan and vulnerability audit
./scripts/ci/check-security.sh

# 5. Unit and race tests with coverage
./scripts/ci/test.sh

# 6. Verify coverage meets baseline (>= 70%)
go test -coverprofile=coverage.out ./...
python3 scripts/ci/check-coverage.py coverage.out
rm -f coverage.out

# 7. Project-specific checks and Docker tag tests
./scripts/ci/test-project.sh

# 8. Snapshot archive packaging and packaged binary smoke test
./scripts/package-snapshot.sh

# 9. Bounded fuzz testing
./scripts/ci/test-fuzz.sh
```
