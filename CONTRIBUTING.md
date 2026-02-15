# Contributing to ojs-backend-nats

Thanks for your interest in contributing.

## Development setup

1. Install Go 1.22+.
2. Start NATS with JetStream enabled:
   ```bash
   nats-server --jetstream
   ```
3. Run checks:
   ```bash
   make test
   make lint
   ```

## Pull request guidelines

- Keep changes focused and small.
- Add or update tests for behavioral changes.
- Update docs when API or behavior changes.
- Ensure `go test ./... -race -cover` passes locally.

## Commit and review expectations

- Use clear commit messages that describe intent.
- Include context in the PR description: problem, approach, and validation performed.
- If a change is breaking, call it out explicitly and update `CHANGELOG.md`.
