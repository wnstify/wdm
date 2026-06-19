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

`make build` writes the binary to `bin/wdm`, and a from-source build reports its version as `dev`. `make e2e` needs a working Docker daemon with the Compose V2 plugin.

GitHub Actions run the required unit, lint, vulnerability, static-analysis, and DCO checks on pull requests and pushes. The Docker end-to-end suite runs on a schedule, on manual dispatch, and when its workflow changes.

## Seed a catalog for local testing

The signed catalog normally arrives through `wdm catalog update`. For local development against a from-source build, stage the in-repo catalog directly:

```sh
make dev-catalog-seed
```

This copies the in-repo catalog into `~/.local/share/wdm/catalogs/` (the `stable/catalog.yaml` manifest plus the shared `templates/` tree) so the dev build can read it. The seeded catalog is unverified and for local development only.

The target refuses to run if a catalog already exists, so it will not clobber a real or verified install. Pass `FORCE=1` to replace an existing catalog:

```sh
make dev-catalog-seed FORCE=1
```

## Test a new app

1. Add the app entry to `catalog/stable/catalog.yaml`.
2. Add its templates under `templates/<app>/` (a `docker-compose.yml.tmpl` and an `.env.tmpl`).
3. Re-seed the local catalog: `make dev-catalog-seed FORCE=1`.
4. Install the new entry against the dev build: `./bin/wdm install <app>`.

Major functional changes must add or update automated tests for the changed behavior. If a change cannot be covered by an automated test, explain the reason in the pull request and describe the manual verification performed.

## Project layout

`wdm` exposes a single public Go API in `pkg/engine`, backed by private packages under `internal/`. The UI layers go through `pkg/engine` rather than reaching into the internal packages directly: the CLI lives in `internal/cli`, and the TUI lives in `internal/tui`. Keep that boundary intact when adding features, so both UIs share one engine.

## Pull requests

1. Fork the repository and create a topic branch for your change.
2. Keep commits focused and atomic: one logical change per commit, with a clear message.
3. Sign off every commit with `git commit -s` to certify the Developer Certificate of Origin.
4. Run `make lint` and `make test` before opening a pull request.
5. Open a pull request against the default branch (`main`) describing what changed and why.

## Developer Certificate of Origin

Every commit must include a `Signed-off-by:` trailer. The signoff certifies that you have the right to contribute the change under this project's license. Use:

```sh
git commit -s
```

For an existing commit, amend it with:

```sh
git commit --amend --signoff
```

The DCO check rejects pull requests with unsigned commits.

## A note on `PRD §N` references

Some code comments reference anchors such as `PRD §29`. These point to an internal product specification that defines the product, security, and architecture decisions behind `wdm`. That specification is not part of this repository. The anchors are stable references that maintainers use, and readers can treat them as informational.

## Code of conduct and security

This project follows our [Code of Conduct](CODE_OF_CONDUCT.md). Please report security issues as described in [SECURITY.md](SECURITY.md) rather than in public issues.

The from-source build and `make dev-catalog-seed` flow is for development only. It does not exercise signed-release verification on install. Production trust comes from the signed installer and `wdm catalog update`, which is fail-closed: a missing or invalid signature stops the update. Never use a hand-seeded catalog as a production trust source.

For the security design, trust boundaries, external interfaces, and assessed risks, see [SECURITY-DESIGN.md](SECURITY-DESIGN.md).
