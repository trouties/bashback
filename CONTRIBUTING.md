# Contributing to bashback

Thanks for your interest. Before proposing features, please read the
**What bashback does not cover** section of the README — many "missing features" are deliberate
honesty about what a content-level git snapshot can and cannot cover.

## Build & test

Requires Go 1.27+ and git >= 2.32.

```sh
go build ./cmd/bashback
go test -race ./...        # unit + integration (real git) + concurrency
go vet ./...
scripts/acceptance.sh      # end-to-end smoke against the built binary
```

## Commits

Format: `<type>(<scope>): <subject>` — e.g. `fix(diff): …`, `feat(hook): …`,
`docs(readme): …`. Keep subjects imperative and under ~70 chars.

## Pull requests

- One logical change per PR, with tests for any behavior change.
- `go test -race ./...` must pass; CI runs Linux + macOS.
- Describe what changed and how you verified it (the PR template asks).
