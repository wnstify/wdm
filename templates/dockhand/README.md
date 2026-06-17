# Dockhand

<p align="center">
  <a href="https://dockhand.pro/">Website</a> •
  <a href="https://dockhand.pro/docs">Documentation</a> •
  <a href="https://github.com/Finsys/dockhand">GitHub</a>
</p>

---

[Dockhand](https://dockhand.pro/) is a modern, security-focused Docker management UI — a self-hosted alternative to Portainer with free SSO/OIDC, zero telemetry, vulnerability scanning, and a visual Compose editor with Git integration.

## Features

- **Container Management** — Start, stop, restart, and remove with real-time CPU/memory monitoring
- **Compose Editor** — Visual Docker Compose editor with Git integration and webhook deployments
- **Web Terminal** — Interactive terminal and file browser (no SSH needed)
- **Vulnerability Scanning** — Grype/Trivy integration scans images before auto-updates
- **SSO/OIDC & MFA** — Free on all tiers
- **Zero Telemetry** — No cloud dependencies, fully self-contained

## Prerequisites

- Docker and Docker Compose
- External Docker networks (`dockhand-frontend`, `dockhand-database`, `dockhand-socket`)
- Reverse proxy (Caddy, Nginx, Traefik) for public TLS

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `dockhand-frontend` network plus `dockhand-database` and `dockhand-socket` (both `--internal`, no internet egress), generates the `PG_PASS` secret via `crypto/rand`, and runs `docker compose up -d` at install time. `PG_PASS` is marked `regenerable=false`: Postgres applies it only at first init, so rotating it would break an already-initialized database — wdm reuses the existing value via `state.ReadStackEnv` on every update. Do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

The stack is three services — a vanilla Postgres backend, a `tecnativa/docker-socket-proxy` sidecar, and the Dockhand server (web UI + API). The reference content below describes the rendered stack for troubleshooting and catalog-template work.

## How Dockhand reaches Docker

Dockhand never touches the raw Docker socket. The `socket-proxy` sidecar is the **only** service that mounts `/var/run/docker.sock` (read-only), and it filters every call against a fixed allow-list. Dockhand reaches the Docker API through the proxy by `DOCKER_HOST=tcp://socket-proxy:2375` over the `--internal` `dockhand-socket` network, which the proxy alone joins — so the Docker API it fronts is never reachable off-host.

The proxy is granted **read AND control** access: list and inspect, plus create, start, stop, restart, and remove containers, images, networks, and volumes. wdm surfaces this as a `DOCKER SOCKET ACCESS WARNING` at install. `SYSTEM` access is intentionally withheld (it only gates `/system/df`, which Dockhand does not use).

## First-run setup

1. **Create the admin account.** Open Dockhand through your reverse proxy (or at
   `http://127.0.0.1:8080`) and create the admin account on first visit. The
   first account becomes the instance owner.

2. **Front it with TLS.** Dockhand serves plain HTTP to its proxy. Point your
   reverse proxy at `http://127.0.0.1:8080` and terminate TLS there.

## Configuration

### Environment Variables

| Variable | Description | Required |
|---|---|---|
| `PG_PASS` | PostgreSQL password (backs the `dockhand` DB superuser) | Yes |
| `PG_USER` | PostgreSQL role / superuser name (default `dockhand`) | No |
| `PG_DB` | Database name (default `dockhand`) | No |
| `TZ` | Container timezone | No (default: host zone) |

`DOCKER_HOST`, `PUID`/`PGID`, and the socket-proxy API flags are catalog-fixed to
the hardened posture this template ships with.

### Reverse Proxy (Caddy)

```caddyfile
dockhand.example.com {
    reverse_proxy http://127.0.0.1:8080
}
```

### Reverse Proxy (Nginx)

```nginx
server {
    listen 443 ssl http2;
    server_name dockhand.example.com;
    ssl_certificate     /etc/letsencrypt/live/dockhand.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/dockhand.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade           $http_upgrade;
        proxy_set_header Connection        "upgrade";
    }
}
```

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 8080 | HTTP | Web interface & API (reverse-proxy target, bound to `127.0.0.1`) |

Only the Dockhand server publishes a port (`127.0.0.1:8080` → container `3000`).
Postgres and the socket-proxy stay on their internal networks and are never
exposed to the host.

## Data Persistence

| Storage | Description |
|------|-------------|
| `dockhand_postgres` (named volume) | PostgreSQL 18 datadir (`/var/lib/postgresql/data`) — postgres self-chowns it on init |
| `dockhand_data` (named volume) | Dockhand application data: settings, stored Compose projects, scan results (`/app/data`) |

Both are **named volumes**, not host bind mounts: wdm does not pre-create host
bind directories, and each service's entrypoint self-chowns its volume on first
boot. wdm never runs `docker compose down -v`, so removing the stack
**preserves** both volumes — they are listed in the removal summary.

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Docker socket | `socket-proxy` is the SOLE `docker.sock:ro` mount; Dockhand uses `DOCKER_HOST=tcp://socket-proxy:2375` | Dockhand never holds the raw socket; the API is filtered |
| Socket reach | The proxy joins `dockhand-socket` (`--internal`) only | The Docker API it fronts is never reachable off-host |
| Capabilities | `cap_drop: ALL` (postgres + dockhand add 4 init caps each; the proxy adds NONE) | No NET/SYS caps anywhere |
| Run-as user | postgres `999`, dockhand `1000` (PUID/PGID), proxy image user | No service runs as root past init |
| Privileges | `security_opt: no-new-privileges` on all containers | Setuid binaries cannot gain caps |
| IPC | `ipc: private` on all containers | Isolated SysV/POSIX IPC namespace |
| Process budget | per-container `pids` limits | Caps fork sprawl |
| Memory / CPU | Per-container limits | One service can't starve the others |
| Three-network split | `dockhand-database` + `dockhand-socket` created with `--internal` | Postgres and the socket path have no internet egress |
| Port exposure | `127.0.0.1:8080` only | Only the reverse proxy can reach Dockhand |
| Healthchecks | `pg_isready` / `wget /_ping` / `curl /` | Built-in checks only |

> **Why does the proxy get write/control access?** Dockhand is a Docker
> *management* UI, so it must create, start, stop, restart, and remove
> containers — not just read state. The `POST` flag (plus the lifecycle and
> delete flags) grants that through the proxy's fixed allow-list, while the proxy
> still blocks anything outside the enabled API groups, runs read-only against
> the socket, and stays on the `--internal` socket network. `SYSTEM` is withheld.

## Support the Project

- ⭐ [Star on GitHub](https://github.com/Finsys/dockhand)
- 📖 [Documentation](https://dockhand.pro/docs)

## License

See the [Dockhand repository](https://github.com/Finsys/dockhand) for licensing.
