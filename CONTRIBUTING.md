# Contributing to Antigravity Proxy

Thank you for your interest in contributing! This document provides guidelines for contributing code, tests, and documentation.

## Code Standards

- Follow idiomatic Go guidelines ("Clear is better than clever").
- Keep happy paths at minimal indentation using early returns.
- Always propagate `context.Context` and respect cancellation.
- Never log, expose, or commit secrets, credentials, or tokens.
- Keep `_test.go` cleanup scoped with `t.Cleanup` or `defer`.
- Format all Go files with `gofumpt` and `goimports`.

## Pull Request Conventions

We follow [Conventional Commits](https://www.conventionalcommits.org/):
- `feat:` for new features (triggers a minor version bump in v0 phase)
- `fix:` for bug fixes (triggers a patch version bump in v0 phase)
- `docs:`, `chore:`, `test:`, `refactor:`, `perf:` for maintenance without version bump

PR titles must match this convention.

## Local Development Workflow

Before submitting a pull request, ensure all local checks pass:

```sh
# 1. Run all quality checks (formatting, vet, golangci-lint)
./scripts/ci/check-quality.sh

# 2. Run shell & Python script syntax validation
./scripts/ci/check-scripts.sh

# 3. Check documentation links and code fences
./scripts/ci/check-docs.sh

# 4. Run secret scan & vulnerability checks
./scripts/ci/check-security.sh

# 5. Run tests with race detector
./scripts/ci/test.sh

# 6. Verify statement coverage meets threshold (>= 70%)
go test -coverprofile=coverage.out ./...
python3 scripts/ci/check-coverage.py coverage.out
rm -f coverage.out

# 7. Run project validation & Docker tag tests
./scripts/ci/test-project.sh
```

## Running Fuzz and Concurrency Stress Tests

To reproduce or run bounded fuzz tests:

```sh
FUZZ_TIME=30s ./scripts/ci/test-fuzz.sh
```

To run concurrency stress tests with race detector and test shuffling:

```sh
STRESS_COUNT=5 ./scripts/ci/test-stress.sh
```
