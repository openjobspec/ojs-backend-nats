# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.0](https://github.com/openjobspec/ojs-backend-nats/compare/v0.1.0...v0.2.0) (2026-02-28)


### Features

* add health check endpoint ([3d4fe42](https://github.com/openjobspec/ojs-backend-nats/commit/3d4fe4201d4a11bb833c8999731f74eb425c1621))
* add JetStream consumer config ([1db1b26](https://github.com/openjobspec/ojs-backend-nats/commit/1db1b267ab123249d39393fed355db919b4ae658))


### Bug Fixes

* correct message acknowledgment flow ([30887d6](https://github.com/openjobspec/ojs-backend-nats/commit/30887d6b72f0ba39aca725aa0f23bef8a548382d))
* correct retry backoff calculation for edge cases ([6582d54](https://github.com/openjobspec/ojs-backend-nats/commit/6582d547a9032d460b9d46d9e7d38de9903c85ac))
* handle connection timeout gracefully ([d8d004b](https://github.com/openjobspec/ojs-backend-nats/commit/d8d004bdb224c70573a235621e257a582cfa47a9))


### Performance Improvements

* optimize batch dequeue query ([be9a761](https://github.com/openjobspec/ojs-backend-nats/commit/be9a7617ecb16a1276c62bab8da0b904b81bcabf))
* optimize poll interval with adaptive backoff ([ec5b110](https://github.com/openjobspec/ojs-backend-nats/commit/ec5b110f76c6a11ca861a875cb4cecd34f7ac458))

## [Unreleased]

### Added
- Integration tests for NATS backend push/fetch/ack and retry promotion flows.
- End-to-end API integration test covering create/fetch/ack/job status lifecycle.
- Contributing and security documentation.
- Issue and pull request templates.
- Tag-based GitHub release workflow that publishes binaries.

### Changed
- Conflict errors now use the `conflict` error code.
- Scheduler stop is now idempotent to avoid double-close panics.
- Improved error handling in key backend and scheduler state transitions.
