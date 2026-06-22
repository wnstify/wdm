# Project Governance

This document defines how the `wdm` project is run: the roles, who holds them, how decisions are made, and how the project continues over time. It complements [CONTRIBUTING.md](CONTRIBUTING.md) (how to contribute), [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) (expected behavior), and [SECURITY.md](SECURITY.md) (vulnerability reporting).

## Model

`wdm` uses a small maintainer-led model. A maintainer team reviews and approves changes, and the project owner holds final decision authority and merge rights. Decisions are made by maintainer consensus in public pull requests and issues; the owner decides when consensus is not reached. The model is intentionally lightweight to match the size of the project, and it is reviewed as the project grows.

## Roles and responsibilities

### Project owner

The owner has administrative control of the repository and final authority over the project.

Responsibilities:

- Final decision on direction, scope, and disputed changes.
- Sole merge rights into the protected `dev` and `main` branches.
- Control of repository settings, branch and tag protection, and rulesets.
- Custody and rotation of release signing material, and publishing of releases.
- Adding and removing maintainers.

The owner is the `wnstfy` GitHub account.

### Maintainers

Maintainers hold Write access and review changes. They are the members of the `@wnstify/webnestify-dev` team, which is the code-owner team in [CODEOWNERS](CODEOWNERS) and is auto-requested to review every pull request.

Responsibilities:

- Review and approve pull requests; every change requires an approving review from a maintainer other than the author.
- Triage issues and security reports.
- Keep documentation and tests in sync with changes.
- Uphold the Code of Conduct.

Maintainers approve changes but do not merge into `dev` or `main`; merging is restricted to the owner by branch protection. This keeps an independent human review on every change, including the owner's own pull requests.

The current maintainers and their review scope are recorded in [CODEOWNERS](CODEOWNERS); team membership is visible on the `@wnstify/webnestify-dev` team page. Maintainer responsibilities for security and release material are detailed in [SECURITY-DESIGN.md](SECURITY-DESIGN.md).

### Contributors

Anyone may contribute. Contributors open issues and pull requests following [CONTRIBUTING.md](CONTRIBUTING.md). Every commit must carry a Developer Certificate of Origin `Signed-off-by:` trailer and be signed; the DCO and required CI checks gate every pull request.

## Decision-making

- Routine changes are decided by maintainer review and the required checks. A pull request merges once required CI passes, conversations are resolved, and a maintainer other than the author has approved the latest push.
- Larger or contested decisions are discussed in the relevant issue or pull request. Maintainers seek consensus; the owner decides if consensus is not reached.
- Changes to security policy, release signing, repository settings, or this governance document are owner decisions.

## Becoming a maintainer

The owner may invite a sustained, trusted contributor to join the `@wnstify/webnestify-dev` team. Maintainer access follows least privilege and is reviewed as described in [SECURITY-DESIGN.md](SECURITY-DESIGN.md#collaborator-access-review). Access is removed when it is no longer needed.

## Continuity

The project is owned within the `wnstify` GitHub organization and has more than one maintainer, so issue triage, change review, and merges can continue if any one person becomes unavailable. Organization owners retain administrative access to the repository, its settings, and its release pipeline. Continuity of release signing depends on the owner's custody of the release signing key; see [SECURITY.md](SECURITY.md#secrets-and-credentials) for how that material is stored and rotated.

## Changing this document

Changes to project governance are made through a pull request to this file and take effect when merged by the owner under the normal review and protection rules.
