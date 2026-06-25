# AppFlowy

<p align="center">
  <a href="https://appflowy.io/">Website</a> •
  <a href="https://docs.appflowy.io/">Documentation</a> •
  <a href="https://github.com/AppFlowy-IO/AppFlowy-Cloud">GitHub</a>
</p>

---

[AppFlowy](https://appflowy.io/) is an open-source workspace for notes, wikis, projects, and databases — a self-hosted alternative to Notion. This template runs the full AppFlowy Cloud backend so the desktop, mobile, and web clients can sync to your own server.

## Services

Nine containers make up the stack:

- **postgres** — Postgres 18 + pgvector, the relational store (named volume).
- **dragonfly** — Redis-compatible cache, password-protected (named volume).
- **minio** — S3-compatible object storage for attachments (named volume).
- **gotrue** — authentication service (login, signup, JWT).
- **appflowy_cloud** — the backend API + WebSocket server.
- **appflowy_worker** — background tasks (imports, etc.).
- **appflowy_web** — the web UI.
- **admin_frontend** — the admin console.
- **angie** — the internal reverse proxy that fronts all of the above.

## Networking

The stack publishes a single port: **`127.0.0.1:8025`** on angie. Front it with your own reverse proxy (Caddy, Nginx, Traefik, Pangolin) terminating TLS at the domain you configured. The proxy must forward WebSocket upgrades on `/ws` and set the `Host` header.

The database, cache, and storage networks are created `--internal`, so postgres, dragonfly, and minio have no internet egress.

## First login

The admin account is `admin@<your-domain>`. The generated password is stored in `~/docker/appflowy/.env` (`GOTRUE_ADMIN_PASSWORD`) — read it there and sign in through the admin console at `/console`.

`GOTRUE_MAILER_AUTOCONFIRM` defaults to `true`, so admin login and new-user signup work with **no SMTP server configured**. New accounts are confirmed automatically.

## User overlay extras (`.env.user`)

SMTP and OAuth are intentionally omitted from the rendered config. To enable them, add the keys below to `~/docker/appflowy/.env.user` (a user-owned overlay wdm never regenerates), then restart the stack.

### Enable transactional email (SMTP)

GoTrue (auth emails — confirmation, recovery, invites):

```sh
GOTRUE_SMTP_HOST=smtp.example.com      # your SMTP server host
GOTRUE_SMTP_PORT=465                   # 465 (TLS wrapper) or 587 (STARTTLS)
GOTRUE_SMTP_USER=noreply@example.com   # SMTP auth username
GOTRUE_SMTP_PASS=...                   # SMTP auth password
GOTRUE_SMTP_ADMIN_EMAIL=admin@example.com  # From: address for auth mail
```

AppFlowy Cloud (app emails — workspace invites, notifications):

```sh
APPFLOWY_MAILER_SMTP_HOST=smtp.example.com
APPFLOWY_MAILER_SMTP_PORT=465
APPFLOWY_MAILER_SMTP_USERNAME=noreply@example.com
APPFLOWY_MAILER_SMTP_EMAIL=noreply@example.com    # From: address
APPFLOWY_MAILER_SMTP_PASSWORD=...
APPFLOWY_MAILER_SMTP_TLS_KIND=wrapper             # wrapper (465) or starttls (587)
```

With SMTP configured you may also set `GOTRUE_MAILER_AUTOCONFIRM=false` to require email confirmation on signup.

### Enable OAuth providers

Set the matching `_ENABLED` flag to `true` and supply the provider credentials:

```sh
GOTRUE_EXTERNAL_GOOGLE_ENABLED=true
GOTRUE_EXTERNAL_GOOGLE_CLIENT_ID=...
GOTRUE_EXTERNAL_GOOGLE_SECRET=...
GOTRUE_EXTERNAL_GOOGLE_REDIRECT_URI=https://<your-domain>/gotrue/callback

GOTRUE_EXTERNAL_GITHUB_ENABLED=true
GOTRUE_EXTERNAL_GITHUB_CLIENT_ID=...
GOTRUE_EXTERNAL_GITHUB_SECRET=...
GOTRUE_EXTERNAL_GITHUB_REDIRECT_URI=https://<your-domain>/gotrue/callback

GOTRUE_EXTERNAL_DISCORD_ENABLED=true
GOTRUE_EXTERNAL_DISCORD_CLIENT_ID=...
GOTRUE_EXTERNAL_DISCORD_SECRET=...
GOTRUE_EXTERNAL_DISCORD_REDIRECT_URI=https://<your-domain>/gotrue/callback
```

### Open signup

Signup is disabled by default (`GOTRUE_DISABLE_SIGNUP=true` in the rendered `.env`). To allow open registration, add to `.env.user`:

```sh
GOTRUE_DISABLE_SIGNUP=false
```
