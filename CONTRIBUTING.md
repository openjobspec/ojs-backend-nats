# Contributing to ojs-backend-nats

Thanks for your interest in contributing!

## Prerequisites

- Go 1.22+
- Docker (for NATS server)
- NATS CLI (optional, for manual testing)

## Local Development Setup

```bash
# Clone the repository
git clone https://github.com/openjobspec/ojs-backend-nats.git
cd ojs-backend-nats

# Start dependencies (NATS with JetStream)
make docker-up

# Build
make build

# Run tests
make test

# Run linter
make lint
```

### Running Locally

```bash
# With Docker Compose (recommended)
make docker-up

# Or start NATS manually with JetStream
nats-server --jetstream

# Then run the server
NATS_URL=nats://localhost:4222 make run
```

### Development with Hot Reload

```bash
# Local hot reload (requires air: go install github.com/air-verse/air@latest)
make dev

# Or via Docker with hot reload
make docker-dev
```

## Architecture

The project uses a three-layer architecture:

- **`internal/api/`** — HTTP and gRPC handlers (chi router)
- **`internal/core/`** — Business logic interfaces and types
- **`internal/nats/`** — NATS JetStream + KV backend implementation
- **`internal/scheduler/`** — Background job promotion and reaping

See the [README](README.md) for the full architecture diagram.

## Development Workflow

1. Fork the repository
2. Create a feature branch from `main`: `git checkout -b feat/my-feature`
3. Make your changes
4. Run tests and linter: `make test && make lint`
5. Commit using [Conventional Commits](https://www.conventionalcommits.org/):
   - `feat:` for new features
   - `fix:` for bug fixes
   - `docs:` for documentation changes
   - `refactor:` for code refactoring
   - `test:` for test additions/changes
   - `chore:` for maintenance tasks
6. Push to your fork and open a Pull Request

## Code Guidelines

- Follow existing code patterns and conventions
- Add tests for new functionality
- Use `log/slog` for structured logging (not `log.Printf`)
- Handle errors explicitly; do not silently ignore them
- Use JetStream for durable message delivery, KV store for job state

## Testing

```bash
make test           # Run all tests with race detector and coverage
make lint           # Run golangci-lint
make conformance    # Run OJS conformance tests (requires running server)
```

## Pull Request Guidelines

1. Create a topic branch from `main`.
2. Keep changes scoped and include tests for behavior changes.
3. Ensure build and tests pass locally.
4. If a change is breaking, call it out explicitly and update `CHANGELOG.md`.
5. Open a PR with a clear description of the change and motivation.

## Reporting Issues

- Use the [bug report template](.github/ISSUE_TEMPLATE/bug_report.yml) for bugs
- Use the [feature request template](.github/ISSUE_TEMPLATE/feature_request.yml) for new ideas

## License

By contributing, you agree that your contributions will be licensed under the Apache-2.0 License.
