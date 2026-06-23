# Mira (Miracode)

<p align="center">
  <a href="https://miracode.ai/">Website</a> •
  <a href="https://docs.miracode.ai/">Documentation</a> •
  <a href="https://github.com/miracodeai/mira">GitHub</a>
</p>

---

[Mira](https://miracode.ai/) is an open-source AI code reviewer that runs as a self-hosted GitHub App. It indexes your repository, reviews pull requests with inline comments and walkthroughs using an LLM of your choice, scans for vulnerabilities, and learns from feedback. No telemetry, no phone-home.

## Features

- **PR Review** — Inline comments + walkthroughs on every pull request
- **Vulnerability Scanning** — Flags security issues in the diff
- **Custom Rules** — Per-repo and global review rules
- **Bring Your Own Model** — OpenRouter, Bedrock, or direct provider keys
- **Self-Hosted** — Your code and credentials never leave your server

## Prerequisites

- Docker and Docker Compose
- A public HTTPS endpoint (Pangolin or your own reverse proxy) so GitHub can deliver webhooks
- A registered GitHub App (see below)
- An OpenRouter API key (or Bedrock/direct provider credentials)

## Setup guide

Mira needs credentials that only you can create, so the install is wired in a
specific order. wdm deploys the stack first; Mira boots and tolerates not-yet-valid
credentials, then sees your installation on the next restart.

### 1. Register the GitHub App

1. GitHub → Settings → Developer settings → GitHub Apps → New GitHub App.
2. Permissions: **Pull requests R/W**, **Contents R/W**, **Issues R/W**, Metadata read.
3. Subscribe to **Pull request** webhook events.
4. Webhook URL: `https://<your-public-host>/github/webhook`.
5. Generate a **webhook secret** and keep it.
6. Generate and **download the private key (PEM)**; note its host path.
7. Record the **App ID**.
8. Install the App on the repositories you want reviewed.

### 2. Get an OpenRouter key

Create a key at [openrouter.ai](https://openrouter.ai/). Bedrock (AWS credentials)
and direct provider keys are alternatives — see the upstream docs.

### 3. Install with wdm

```
wdm apps install mira --domain mira.example.com \
  --set MIRA_GITHUB_APP_ID=<app-id> \
  --set MIRA_WEBHOOK_SECRET=<webhook-secret> \
  --set OPENROUTER_API_KEY=<openrouter-key> \
  --set MIRA_PEM_PATH=/absolute/path/to/github-app.pem
```

`mira` carries the `database` risk tag, so install gates interactively (it is not
auto-installed under `--yes`). wdm generates `POSTGRES_PASSWORD` and `ADMIN_PASSWORD`
itself and mounts the PEM read-only at `/keys/github-app.pem`.

### 4. Reverse proxy / ingress

Mira binds `127.0.0.1:8000` only. Point your reverse proxy (or Pangolin) at it and
confirm the live public URL matches the webhook URL set in step 1.

### 5. Models

Log in to the dashboard as **`admin`** (password = `ADMIN_PASSWORD`, surfaced at
install) and open **Settings → Models**. The shipped default review model is
`anthropic/claude-sonnet-4-6` (reliable structured output); a cheaper indexing
model such as `anthropic/claude-haiku-4-5` is a good pairing. Changing the indexing
model after the first index warrants a re-index.

### 6. Verify

Open a test pull request and confirm Mira comments. Check `wdm apps status mira`
and `docker logs mira` if a review does not appear (common causes: webhook URL or
secret mismatch, HTTP vs HTTPS, a 401 from an App ID / key mismatch, or a bad model
string / no provider credits).

## Configuration

### Environment Variables

| Variable | Description | Source |
|---|---|---|
| `POSTGRES_USER` / `POSTGRES_DB` | DB role + database (catalog-fixed: `mira`) | Fixed |
| `POSTGRES_PASSWORD` | Postgres password | wdm-generated |
| `ADMIN_PASSWORD` | Dashboard admin password (username `admin`) | wdm-generated |
| `DATABASE_URL` | Postgres connection URL (derived) | Fixed |
| `MIRA_GITHUB_APP_ID` | GitHub App ID | `--set` |
| `MIRA_GITHUB_PRIVATE_KEY` | `@/keys/github-app.pem` (the mounted PEM) | Fixed |
| `MIRA_PEM_PATH` | Host path to the PEM, mounted read-only | `--set` |
| `MIRA_WEBHOOK_SECRET` | Must match the GitHub App registration | `--set` |
| `OPENROUTER_API_KEY` | LLM provider key | `--set` |
| `MIRA_MODEL` | Primary model (env fallback) | Fixed default |
| `TZ` | Container timezone | Host zone |

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 8000 | HTTP | Dashboard + GitHub webhook receiver (bound to `127.0.0.1`) |

## Data Persistence

| Storage | Description |
|------|-------------|
| `db_storage` (named volume) | PostgreSQL data — the code-index/review engine DB |
| `indexes_storage` (named volume) | Index store + the dashboard `_app.db` (admin login, model settings, custom rules, review progress) |

Both are **named volumes**, not host bind mounts. The index store is **not** a
rebuildable cache — the dashboard admin and settings live there — so wdm preserves
both volumes on remove (wdm never runs `docker compose down -v`).

## Security Features

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL` (postgres adds 5 init caps; mira adds none) | mira runs with zero added capabilities |
| Privileges | `security_opt: no-new-privileges` on all containers | Setuid binaries cannot gain caps |
| IPC | `ipc: private` on all containers | Isolated SysV/POSIX IPC namespace |
| Two-network split | `mira-db` created with `--internal` | Postgres has no internet egress |
| Port exposure | `127.0.0.1:8000` only | Only the reverse proxy can reach mira |
| Postgres auth | `SCRAM-SHA-256` | Stronger than the default md5 |
| Private key | Mounted read-only from a host path | mira only ever reads the PEM |

> **Boot tolerates not-yet-valid credentials.** Mira logs a 401 and completes
> startup even before the GitHub App is fully wired, so a deploy-then-install (or
> register-then-update) order both work. It detects installations on every boot.

## License

Mira is released under the [Apache-2.0 License](https://github.com/miracodeai/mira/blob/main/LICENSE).
