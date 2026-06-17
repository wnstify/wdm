# Zulip

<p align="center">
  <a href="https://zulip.com/">Website</a> •
  <a href="https://zulip.readthedocs.io/">Documentation</a> •
  <a href="https://github.com/zulip/zulip">GitHub</a> •
  <a href="https://chat.zulip.org/">Community</a>
</p>

---

[Zulip](https://zulip.com/) is an open-source team chat application with topic-based threading. Conversations are organized by topic within streams, which keeps async discussion readable — a self-hostable alternative to Slack.

## Features

- **Topic-Based Threading** — Conversations organized by topic within streams
- **Powerful Search** — Full-text search (Postgres tsvector by default; pgroonga available as an optional swap)
- **Markdown Support** — Rich formatting, code blocks, LaTeX, syntax highlighting
- **Integrations** — 100+ integrations (GitHub, Jira, GitLab, PagerDuty, and more)
- **Mobile Apps** — iOS, Android, and desktop applications
- **Guest Access** — Invite external collaborators with restricted permissions
- **Self-Hosted** — Full data ownership, no vendor lock-in

## Prerequisites

- Docker and Docker Compose
- External Docker networks (`zulip-front`, `zulip-db`)
- Reverse proxy (Caddy, Nginx, Traefik) for public TLS
- A real fully qualified domain name (Zulip refuses a dotless hostname)

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `zulip-front` network plus `zulip-db` (`--internal`, no internet egress), generates the five secrets (`POSTGRES_PASSWORD`, `MEMCACHED_PASSWORD`, `RABBITMQ_DEFAULT_PASS`, `REDIS_PASSWORD`, `ZULIP_SECRET_KEY`) via `crypto/rand`, and runs `docker compose up -d` at install time. Every secret is marked `regenerable=false`: each backend applies its password only at first init, and the Django signing key (`ZULIP_SECRET_KEY` in `.env`, mapped onto the `SECRETS_secret_key` env var Zulip reads) signs sessions — rotating any of them would break an already-initialized stack, so wdm reuses the existing values via `state.ReadStackEnv` on every update. Do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

The stack is five services — a Postgres backend, a SASL-authenticated memcached session cache, a RabbitMQ event queue, a password-protected Redis rate-limit store, and the all-in-one Zulip server (Django + Tornado + nginx + supervisor in one upstream image). The four backing services live on the `--internal` `zulip-db` network with no internet egress; only the Zulip server reaches the front network. First boot applies ~280 Django migrations against the fresh database, so it takes roughly four to six minutes before the app reports healthy. The reference content below describes the rendered stack for troubleshooting and catalog-template work.

## First-run setup

1. **Set your real admin email.** The generated `.env` defaults
   `SETTING_ZULIP_ADMINISTRATOR` to `admin@example.com`. It cannot reference
   your domain, so open `~/docker/zulip/.env`, set it to the address that should
   receive admin error reports, and re-run the stack.

2. **Create your first organization.** Until you create an organization (realm),
   `/` returns an HTTP 500 — this is expected on a brand-new server. Generate the
   single-use realm-creation link:

   ```bash
   docker exec -it zulip-server \
     /home/zulip/deployments/current/manage.py generate_realm_creation_link
   ```

   Open the printed URL through your reverse proxy to create the organization and
   its first admin user.

3. **Front it with TLS.** The Zulip server serves plain HTTP to its proxy
   (`http_only=True`). Point your reverse proxy at `http://127.0.0.1:8080` and
   terminate TLS there; a real FQDN is required (Zulip rejects a dotless host).

## Configuration

### Environment Variables

| Variable | Description | Required |
|---|---|---|
| `POSTGRES_PASSWORD` | PostgreSQL password (backs the `zulip` DB user) | Yes |
| `MEMCACHED_PASSWORD` | SASL password for Zulip's memcache user | Yes |
| `RABBITMQ_DEFAULT_PASS` | RabbitMQ password (`zulip` user) | Yes |
| `REDIS_PASSWORD` | Redis `requirepass` | Yes |
| `ZULIP_SECRET_KEY` | Django SECRET_KEY (session cookies, CSRF); mapped onto `SECRETS_secret_key` | Yes |
| `SETTING_EXTERNAL_HOST` | Public FQDN (no scheme) | Yes |
| `SETTING_ZULIP_ADMINISTRATOR` | Admin email (default `admin@example.com`) | No |
| `TZ` | Container timezone | No (default: host zone) |
| `SETTING_EMAIL_HOST` / `_HOST_USER` / `_PORT` / `_USE_TLS` | SMTP settings | No |
| `SECRETS_email_password` | SMTP password | No |
| `SETTING_NOREPLY_EMAIL_ADDRESS` | From: address for system mail | No |

`ZULIP_HTTP_ONLY`, `LOADBALANCER_IPS`, `ZULIP_AUTH_BACKENDS`, and
`SETTING_USING_PGROONGA` are catalog-fixed in `.env` to the hardened
reverse-proxy posture this template ships with.

### Reverse Proxy (Caddy)

```caddyfile
chat.example.com {
    encode zstd gzip
    reverse_proxy http://127.0.0.1:8080 {
        header_up X-Real-IP {remote_host}
        header_up X-Forwarded-Proto {scheme}
    }
}
```

### Reverse Proxy (Nginx)

```nginx
server {
    listen 443 ssl http2;
    server_name chat.example.com;
    ssl_certificate     /etc/letsencrypt/live/chat.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/chat.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade           $http_upgrade;
        proxy_set_header Connection        "upgrade";
        proxy_read_timeout 1200s;  # Tornado long-poll
    }
}
```

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 8080 | HTTP | Web interface (reverse-proxy target, bound to `127.0.0.1`) |

Only the Zulip server publishes a port (`127.0.0.1:8080` → container `80`). The
four backing services stay on the internal-only `zulip-db` network and are never
exposed to the host.

## Data Persistence

| Storage | Description |
|------|-------------|
| `zulip_postgres` (named volume) | PostgreSQL 18 datadir (`/var/lib/postgresql/data`) — postgres self-chowns it on init |
| `zulip_rabbitmq` (named volume) | RabbitMQ Mnesia, queues, schema (`/var/lib/rabbitmq`) |
| `zulip_redis` (named volume) | Redis persistence: rate-limit counters (`/data`) |
| `zulip_data` (named volume) | Zulip uploads, custom emoji, secrets cache (`/data`) |

All are **named volumes**, not host bind mounts: wdm does not pre-create host
bind directories, and each service's entrypoint self-chowns its volume on first
boot. wdm never runs `docker compose down -v`, so removing the stack
**preserves** every volume — they are listed in the removal summary.

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL` (postgres adds 4 init caps; zulip adds 6; the other three backends add NONE) | No NET/SYS caps anywhere except `NET_BIND_SERVICE` for Zulip's nginx |
| Run-as user | memcached `11211`, rabbitmq `999`, redis `999:1000` (image users) | The backing services never run as root |
| Privileges | `security_opt: no-new-privileges` on all containers | Setuid binaries cannot gain caps |
| IPC | `ipc: private` on all containers | Isolated SysV/POSIX IPC namespace |
| Process budget | per-container `pids` limits (Zulip's is high — supervisor spawns ~25 workers) | Caps fork sprawl |
| Memory / CPU | Per-container limits | One service can't starve the others |
| Two-network split | `zulip-db` created with `--internal` | The four backing services have no internet egress |
| Port exposure | `127.0.0.1:8080` only | Only the reverse proxy can reach Zulip |
| Backend auth | Each backing service has its own password | Compromise of one does not auto-grant the others |
| Healthchecks | `pg_isready` / `nc` / `rabbitmq-diagnostics` / `redis-cli` / `curl /health` | Built-in checks only |

> **Why does Zulip's server still run as container root?** The upstream image is
> built around supervisord: the PID-1 entrypoint chowns the data volume on first
> boot, runs `cp`/`rm` under `/etc/zulip/`, and `su zulip -c`'s migrations and
> long-running workers down to uid 1000. Those operations need
> CHOWN/SETUID/SETGID/DAC_OVERRIDE/FOWNER, and nginx needs NET_BIND_SERVICE for
> port 80. `cap_drop: ALL` removes every other capability, `no-new-privileges`
> blocks setuid escalation, and `zulip-db` is `--internal`.

> **Vanilla `postgres:18.4` (no pgroonga).** Zulip Server 12 supports PostgreSQL
> 14-18. This template runs vanilla `postgres:18.4` with
> `SETTING_USING_PGROONGA=False` and falls back to Postgres's built-in `tsvector`
> full-text search, which suits Latin-script teams. For CJK / non-Latin search,
> swap to `zulip/zulip-postgresql:14` and set `SETTING_USING_PGROONGA=True`.

> **First boot.** Zulip applies ~280 Django migrations against the fresh
> database on first start, so the container can take four to six minutes to
> report healthy. The compose healthcheck uses a 300s `start_period` for this.

## Support the Project

- ⭐ [Star on GitHub](https://github.com/zulip/zulip)
- 💵 [Sponsor on GitHub](https://github.com/sponsors/zulip)
- 💬 [Community Chat](https://chat.zulip.org/)
- 📖 [Documentation](https://zulip.readthedocs.io/)

## License

Zulip is released under the [Apache-2.0 License](https://github.com/zulip/zulip/blob/main/LICENSE).
