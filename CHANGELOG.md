# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## 1.0.0 (2026-02-16)


### Features

* **admin:** add embedded admin dashboard ([cd08e29](https://github.com/openjobspec/ojs-backend-nats/commit/cd08e29f5f3a835109c78225b977e4eb4c0ad644))
* **api:** add HTTP handlers and middleware ([88b9a7c](https://github.com/openjobspec/ojs-backend-nats/commit/88b9a7ce31117909659af4a66ec28d555025d3ca))
* **core:** add job domain types and 8-state lifecycle ([56a8ddd](https://github.com/openjobspec/ojs-backend-nats/commit/56a8ddd60fa4c723d3b6e157e15b53886b67b41e))
* **grpc:** add gRPC transport layer ([8d65427](https://github.com/openjobspec/ojs-backend-nats/commit/8d65427f0321fce0b0f19e42692ec06828e1e2cf))
* **nats:** add JetStream stream and KV infrastructure ([c60c5cb](https://github.com/openjobspec/ojs-backend-nats/commit/c60c5cbdbcd69047f89ead453daa5036db8ccad3))
* **nats:** implement backend operations and workflows ([3caa70e](https://github.com/openjobspec/ojs-backend-nats/commit/3caa70e52d175e78ade0c16e4bfa691f605fbd6e))
* **scheduler:** add background job scheduler ([22f7cdb](https://github.com/openjobspec/ojs-backend-nats/commit/22f7cdbf92d00dbe80fb44da869fcab3d99e8840))
* **server:** add server config, router, and entry point ([94fa48a](https://github.com/openjobspec/ojs-backend-nats/commit/94fa48aba1a7c600d14925f01fcd67b6f72ec5ac))

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
