# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
