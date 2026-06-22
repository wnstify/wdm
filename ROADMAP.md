# Roadmap

This roadmap describes what `wdm` intends to do, and explicitly not do, over roughly the next year. It is a statement of direction, not a commitment to dates; priorities shift with maintainer capacity and security needs. Live planning happens in [GitHub issues](https://github.com/wnstify/wdm/issues); this file is the higher-level summary.

The current stable release is `v1.0.6`. See [CHANGELOG.md](CHANGELOG.md) for shipped work and [GOVERNANCE.md](GOVERNANCE.md) for how decisions are made.

## Next release — v1.1.0

- **Add Mira to the catalog** ([#49](https://github.com/wnstify/wdm/issues/49)) — an open-source, self-hosted AI code reviewer (Apache 2.0) that runs as a GitHub App and reviews pull requests with a model of the operator's choice. Curated template, localhost-by-default, and the same render/verification gates as every other catalog app.

## Ongoing (continuous, every release)

- **Security first.** Coordinated vulnerability disclosure, fail-closed release verification, and release-blocking SCA/SAST/license gates as documented in [SECURITY.md](SECURITY.md). OpenSSF Best Practices and Scorecard work continues.
- **Dependencies and runtime.** Keep Go, dependencies, and the supported OS/Docker matrix current; track and patch advisories.
- **Catalog maintenance.** Keep curated app templates working against upstream image changes; surface available app updates to users.
- **Documentation in sync.** Keep README, USAGE, SECURITY, and design docs consistent with the shipped binary.

## Under consideration (not committed)

- Additional curated apps requested through issues, evaluated case by case for fit, maintenance cost, and security posture.
- Quality-of-life improvements to the TUI/CLI driven by user-reported issues.

New app requests and feature ideas are triaged in the [issue tracker](https://github.com/wnstify/wdm/issues); acceptance is at maintainer discretion per [GOVERNANCE.md](GOVERNANCE.md).

## Out of scope (not planned)

To keep the project focused and safe, `wdm` does **not** intend to:

- Manage arbitrary Docker Compose projects — only the fixed, curated catalog.
- Support platforms beyond Linux amd64 on the documented OS and Docker matrix.
- Ship more than a single stable release channel.
- Run as root or under sudo, or operate as a network service / daemon.
- Back up application data volumes, or delete user data on removal.
- Provide commercial support or an SLA; support is self-service and community-based.
