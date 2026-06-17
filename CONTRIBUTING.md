# Contributing to wdm

Contributions are welcome. `wdm` is released under the [MIT license](LICENSE); by contributing, you agree that your changes are licensed under the same terms.

## Prerequisites

- Go 1.26 (the toolchain version in [go.mod](go.mod)).
- Docker 20.10+ with Compose V2, required to run the end-to-end tests.
- A Linux amd64 target. `wdm` targets Linux amd64; other platforms are unsupported.

## Build and test

Use the project's Make targets:

| Target | Purpose |
|---|---|
| `make build` | Build for the current OS. |
| `make build-linux` | Build for linux/amd64, the release target. |
| `make test` | Run unit tests. |
| `make test-race` | Run unit tests with the race detector. |
| `make e2e` | Run end-to-end tests against real Docker. |
| `make lint` | Run the linters. |
| `make vet` | Run `go vet`. |
| `make fmt` | Format the code. |
| `make tidy` | Tidy `go.mod` and `go.sum`. |

`make e2e` needs a working Docker daemon with the Compose V2 plugin.

## Project layout

`wdm` exposes a single public Go API in `pkg/engine`, backed by private packages under `internal/`. The UI layers go through `pkg/engine` rather than reaching into the internal packages directly: the CLI lives in `internal/cli`, and the TUI lives in `internal/tui`. Keep that boundary intact when adding features, so both UIs share one engine.

## Pull requests

1. Fork the repository and create a topic branch for your change.
2. Keep commits focused and atomic: one logical change per commit, with a clear message.
3. Run `make lint` and `make test` before opening a pull request.
4. Open a pull request against the default branch (`main`) describing what changed and why.

## A note on `PRD §N` references

Some code comments reference anchors such as `PRD §29`. These point to an internal product specification that defines the product, security, and architecture decisions behind `wdm`. That specification is not part of this repository. The anchors are stable references that maintainers use, and readers can treat them as informational.

## Code of conduct and security

This project follows our [Code of Conduct](CODE_OF_CONDUCT.md). Please report security issues as described in [SECURITY.md](SECURITY.md) rather than in public issues.
