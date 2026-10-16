# Changelog

## [0.2.0](https://github.com/terastal-hq/antigravity-proxy/compare/v0.1.0...v0.2.0) (2026-10-16)


### Features

* add OAuth session management, login account flows, runtime proxy, and status monitoring improvements ([a3c9611](https://github.com/terastal-hq/antigravity-proxy/commit/a3c96111ae981662e66f21c5fe3aa0829942f4c2))
* **ci:** unify CI/CD pipelines, code quality, and security workflows ([78940da](https://github.com/terastal-hq/antigravity-proxy/commit/78940daefb485a5228b22df4ddbef51aa124585e))
* implement multi-account system with Admin API and account key routing ([373e7e9](https://github.com/terastal-hq/antigravity-proxy/commit/373e7e914c738f91e94c0943e7a28191f7f0e113))
* **quota:** synthesize 5-hour window from weekly quota for weekly-only tiers ([fcd18a5](https://github.com/terastal-hq/antigravity-proxy/commit/fcd18a511493b82f5ad693a2af3446375c40aa21))
* scan binary for oauth strings ([7317e82](https://github.com/terastal-hq/antigravity-proxy/commit/7317e821929c26390d897852e122f0c3d8d75704))


### Bug Fixes

* **ci:** update security workflow Go version to 1.27.2 to resolve stdlib CVEs ([62c2c59](https://github.com/terastal-hq/antigravity-proxy/commit/62c2c593bbd60477d47d5160acab2f12a2dc2d29))
* **ci:** update to Go 1.27.2 for stdlib CVEs, fix actionlint warnings and platform test ([f8b9ddd](https://github.com/terastal-hq/antigravity-proxy/commit/f8b9dddf63ff9e40cf5d0c3b828bc691b3c7a16c))
* harden config and startup boundaries ([5e004c7](https://github.com/terastal-hq/antigravity-proxy/commit/5e004c7c7a0898a7d71380a00bd6d92292b3dbce))
* preserve query separators in JSON login URL responses ([0c84d40](https://github.com/terastal-hq/antigravity-proxy/commit/0c84d4018f56bf63e435930b98d86df4fcae7d05))
* preserve query separators in JSON login URL responses ([0360273](https://github.com/terastal-hq/antigravity-proxy/commit/036027313a11f60c0ae7b7536e1ebb75ecc06abd))
* **proxy:** accept per-account keys when global API_KEY is set ([ad27933](https://github.com/terastal-hq/antigravity-proxy/commit/ad27933a793a450a549fd824bf2a791b9cfe95f4)), closes [#7](https://github.com/terastal-hq/antigravity-proxy/issues/7)
