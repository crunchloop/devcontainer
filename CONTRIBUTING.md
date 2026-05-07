# Contributing

Thanks for your interest. This project is in early alpha — the API is not
stable and the public scope is defined by [PRD.md](PRD.md). Issues and PRs
that fall outside the PRD's scope (see §4 Non-goals) will likely be closed
with a pointer to the PRD.

## Dev setup

Requirements:

- Go 1.25+
- Docker (with the Compose v2 plugin if you touch compose code)
- `golangci-lint` v1.64+ (`brew install golangci-lint` or see
  https://golangci-lint.run/welcome/install/)

```bash
git clone git@github.com:crunchloop/devcontainer.git
cd devcontainer
make test          # unit tests
make test-integration   # integration tests (requires Docker daemon)
make lint
```

## Test layout

- Unit tests live next to the code (`*_test.go`).
- Integration tests live under `test/integration/` and are gated by
  `//go:build integration`. They require a real Docker daemon and may pull
  images on first run.

## Pull requests

- Keep PRs focused; one logical change per PR.
- Run `make lint test` locally before pushing.
- Add or update tests for behavior changes.
- Reference the relevant PRD section in the PR description when applicable.

## Reporting issues

Bug reports should include the Go version, Docker version, and (if relevant)
a minimal `devcontainer.json` that reproduces the problem.
