# SerpBear

<p align="center">
  <a href="https://serpbear.com/">Website</a> •
  <a href="https://docs.serpbear.com/">Documentation</a> •
  <a href="https://github.com/towfiqi/serpbear">GitHub</a>
</p>

---

[SerpBear](https://serpbear.com/) is a free and open-source search-engine rank tracker. Track your keywords' Google positions over time, group them by domain, and pull ranking data through a built-in REST API — all self-hosted, with no per-keyword fees.

## Features

- **Keyword Rank Tracking** — Daily Google positions per keyword and domain
- **Search Console Integration** — Import real impressions and clicks (optional)
- **Email & Notifications** — Scheduled reports over SMTP (optional)
- **REST API** — Read ranking data programmatically with a bearer token
- **Mobile-Friendly UI** — Manage domains and keywords from any device
- **Self-Hosted** — Your keyword data stays on your own server

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik)

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `serpbear` network, generates the admin `PASSWORD`, the session `SECRET`, and the REST `APIKEY` via `crypto/rand`, derives `NEXT_PUBLIC_APP_URL` from the single `SERPBEAR_DOMAIN` input, and runs `docker compose up -d` at install time. All three secrets are marked `regenerable=false` — rotating them post-install would invalidate the current login, log sessions out, or break API clients (no data loss in any case), so wdm reuses the existing values via `state.ReadStackEnv` on every update. See the wdm documentation for the CLI surface; do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

After install, open SerpBear at `http://127.0.0.1:3009` (or via your reverse proxy) and sign in. **Your admin login is the configured `USER_NAME` (default `admin`); the generated password is the `PASSWORD=` line in `~/docker/serpbear/.env`.** Then add a domain, add keywords, and (optionally) connect Google Search Console for real ranking data. The reference content below describes the rendered stack (configuration knobs, security baseline, data layout) for troubleshooting and catalog-template work.

## Configuration

### Environment Variables

| Variable | Description | Required |
|---|---|---|
| `USER_NAME` | Dashboard login username | No (default: `admin`) |
| `PASSWORD` | Dashboard login password (wdm-generated) | Yes |
| `SECRET` | Session JWT signing secret | Yes |
| `APIKEY` | REST API bearer token | Yes |
| `SESSION_DURATION` | Session lifetime in hours | No (default: `24`) |
| `NEXT_PUBLIC_APP_URL` | Public URL (derived from `SERPBEAR_DOMAIN`) | Yes |
| `TZ` | Container timezone | No (default: host timezone) |

Optional Google Search Console (`SEARCH_CONSOLE_*`) and SMTP (`SMTP_*`)
settings are documented as commented hints in `.env.tmpl` — fill them in the
template (not the rendered `.env`, which wdm overwrites on update) to enable
real ranking data imports or email notifications.

### Reverse Proxy (Caddy)

```
serpbear.example.com {
    reverse_proxy http://localhost:3009
}
```

`SERPBEAR_DOMAIN` must be set to this public hostname so `NEXT_PUBLIC_APP_URL`
matches the origin the browser actually uses — otherwise CSRF checks, OAuth
callbacks, and notification links break.

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 3009 | HTTP | Web interface (bound to `127.0.0.1`) |

## Data Persistence

| Storage | Description |
|------|-------------|
| `serpbear_data` (named volume) | `database.sqlite` + `settings.json` (`/app/data`) |

`serpbear_data` is a **named volume**, not a host bind mount. The serpbear
image runs as the unprivileged `nextjs` user (UID 1001) with `cap_drop: ALL`
and no chowning entrypoint, so a bind mount — whose host source Docker
auto-creates root-owned — fails the SQLite open with `SQLITE_CANTOPEN` (the
healthcheck still passes, but the database silently never opens). A named
volume is seeded `nextjs`-owned from the image, so the database opens and
migrations run cleanly. wdm never runs `docker compose down -v`, so removing
the stack **preserves** `serpbear_data` (listed in the removal summary as
`wdm-serpbear_serpbear_data`), and your tracked keywords survive a remove.

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL`, no caps added | Clean non-root; no NET/SYS caps |
| User | Image-default `nextjs` (UID 1001), no `user:` override | Server + SQLite run unprivileged |
| Privileges | `security_opt: no-new-privileges` | Setuid binaries cannot gain caps |
| IPC | `ipc: private` | Isolated SysV/POSIX IPC namespace |
| Process budget | `pids: 200` | Caps fork sprawl; fork-bomb resistance |
| Memory / CPU | Per-container limits | Won't starve other stacks |
| Port exposure | `127.0.0.1:3009:3000` | Only the reverse proxy can reach serpbear |
| Data volume | Named volume (`serpbear_data`) | SQLite opens nextjs-owned; survives remove |
| Healthcheck | `wget` app root (no creds on cmdline) | Built-in busybox probe only |
| Ephemeral writes | `tmpfs` for `/tmp` (128 MiB) | Scratch stays in RAM, never hits disk |

> **Why no `user:` directive?** The serpbear image already runs as the
> non-root `nextjs` user (UID 1001) that owns `/app`. Forcing a different
> UID (for example `user: "1000:1000"`) leaves the healthcheck passing but
> the SQLite database silently failing to open (`SQLITE_CANTOPEN`) — verified
> in testing. Running as the image default is the only configuration that
> opens the database and runs migrations cleanly.

## Support the Project

- ⭐ [Star on GitHub](https://github.com/towfiqi/serpbear)
- 🐛 [Report Issues](https://github.com/towfiqi/serpbear/issues)
- 📖 [Documentation](https://docs.serpbear.com/)

## License

SerpBear is released under the [MIT License](https://github.com/towfiqi/serpbear/blob/main/LICENSE).
