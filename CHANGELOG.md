# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.5.0] - 2026-09-02

### Changed
- Upgraded gRPC, OpenTelemetry, Chi, and Go networking dependencies to patched
  releases, migrated WebSockets to `coder/websocket`, and raised Go to 1.25.
- Added revision-fenced state transitions, durable transport handoffs,
  distributed cron ownership, and stricter gRPC behavior.
- Aligned release dependencies and hardened standalone CI, release artifacts,
  checksums, and provenance for the coordinated 0.5.0 train.

## [0.4.1] - 2026-04-21

### Fixed
- Added deterministic scheduler shutdown and closed a cancellation leak.
- Propagated previously ignored JSON marshaling and conversion errors.

## [0.4.0] - 2026-04-20

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
