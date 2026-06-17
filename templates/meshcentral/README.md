# MeshCentral

<p align="center">
  <a href="https://meshcentral.com/">Website</a> •
  <a href="https://ylianst.github.io/MeshCentral/">Documentation</a> •
  <a href="https://github.com/Ylianst/MeshCentral">GitHub</a> •
  <a href="https://www.youtube.com/channel/UCJWzaCpdgNgEhUyk2HhFNFQ">Videos</a>
</p>

---

[MeshCentral](https://meshcentral.com/) is an open-source, self-hosted remote
monitoring and management platform — a self-hosted alternative to TeamViewer and
ConnectWise. Manage computers over the web: remote desktop, terminal, file
transfer, wake-on-LAN, and a software agent for Windows, macOS, and Linux.

## Features

- **Remote Desktop / Terminal / Files** — Manage devices through the browser
- **Self-Hosted Agent** — Cross-platform mesh agent, no third-party relay
- **Wake-on-LAN & Power Control** — Bring machines up remotely
- **Two-Factor Auth** — TOTP, email, and hardware-key support
- **Multi-Tenant Domains** — Isolate device groups per domain
- **Self-Hosted** — Full control over your data and the management plane

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik) terminating TLS

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl`,
`.env.tmpl`, and `config.json.tmpl` into your stack directory, pre-creates the
`meshcentral-front` network plus `meshcentral-db` (`--internal`, no internet
egress), and runs `docker compose up -d` at install time. MeshCentral is the
first wdm app to use **config generation**: wdm renders `config.json` itself
(replacing the upstream `generate-config.sh`) from the placeholder map and
bind-mounts it read-only into the container. The rendered `config.json` is
authoritative — see the note below. wdm generates no secrets for this stack
(MongoDB runs without auth on the internal network; MeshCentral persists its own
session secret in its data volume). See the wdm documentation for the CLI
surface; do not edit the rendered files by hand, since wdm regenerates them on
update.

After install, open MeshCentral through your reverse proxy and create the first
account — the first account registered becomes the site administrator. First
boot generates a self-signed certificate and initializes the MongoDB collections,
so the server can take up to a minute to report healthy. The reference content
below describes the rendered stack (configuration knobs, security baseline, data
layout) for troubleshooting and catalog-template work.

## No host kernel modules required

Unlike some remote-access stacks, MeshCentral needs **no** host `modprobe`,
no kernel module, and no `/lib/modules` mount. It is a plain Node.js HTTP server;
the mesh agent runs on the managed endpoints, not on the host. The stack runs
entirely within the container privilege baseline below.

## Reverse proxy and TLS offload

MeshCentral runs in **tlsOffload** mode: it serves plain HTTP on container port
443 (published to `127.0.0.1:4430`) and trusts the reverse proxy to terminate
TLS. `trustedProxy` is enabled so MeshCentral reads the real client IP from the
proxy's forwarded headers, and `redirPort` is set to `0` to disable the
container's own HTTP→HTTPS redirect (the proxy owns that). Point your proxy at
`http://127.0.0.1:4430` with WebSocket upgrade support enabled — MeshCentral's
agent and web console rely on long-lived WebSocket connections.

## Configuration

### config.json (wdm-rendered)

wdm renders `config.json` from `config.json.tmpl`. It is a non-secret file (mode
`0644`) and is mounted **read-only** over the data volume. Key settings:

| Setting | Value | Effect |
|---|---|---|
| `cert` | `MESHCENTRAL_DOMAIN` | Hostname on the generated certificate / domain |
| `port` | `443` | Container HTTP port (published to `127.0.0.1:4430`) |
| `redirPort` | `0` | HTTP→HTTPS redirect disabled (reverse proxy owns it) |
| `tlsOffload` | `true` | Serve plain HTTP; proxy terminates TLS |
| `trustedProxy` | `true` | Trust forwarded client-IP headers |
| `mongoDb` | `mongodb://mongodb:27017/meshcentral` | MongoDB connection (no auth) |
| `WebRTC` | `false` | WebRTC peer connections off (proxy-only deployment) |
| `SelfUpdate` | `false` | In-container self-update off — wdm owns image updates |
| `domains."".NewAccounts` | `true` | Allow account creation (first account is admin) |

> **The rendered config.json is authoritative.** The MeshCentral image only
> rewrites `config.json` when `DYNAMIC_CONFIG=true` or the
> `HOSTNAME` / `PORT` / `USE_MONGODB` / `MONGO_URL` environment variables are
> set. This template passes **none** of those (`DYNAMIC_CONFIG` is left at the
> image default `false`), so the entrypoint reads wdm's file verbatim and never
> clobbers it. Once you have your first admin account, you can disable open
> registration by setting `NewAccounts` to `false`.

### Environment Variables

| Variable | Description | Required |
|---|---|---|
| `MESHCENTRAL_DOMAIN` | Public hostname (rendered into `config.json`) | Yes |
| `TZ` | Container timezone | No (default: host zone) |

## MongoDB runs without authentication

The `mongodb` service runs with **no authentication** — no
`MONGO_INITDB_ROOT_USERNAME` / `MONGO_INITDB_ROOT_PASSWORD`. This is safe and
intentional here:

- It is attached **only** to `meshcentral-db`, created with `--internal`, so it
  has no internet egress and binds no host port.
- MeshCentral is the sole client on that network.
- It keeps the rendered `config.json` free of any database credential.

This mirrors the upstream MeshCentral compose. The image is pinned to
**`mongo:6`**: MeshCentral bundles the `mongodb@4.17.2` Node driver, which
supports MongoDB server versions up to 6 — do **not** bump to `mongo:7` or
`mongo:8`.

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 4430 | HTTP | Web console + agent endpoint (bound to `127.0.0.1`) |

The container listens on 443 in `tlsOffload` mode (plain HTTP); wdm publishes it
on `127.0.0.1:4430` so only the reverse proxy can reach it.

## Data Persistence

| Storage | Description |
|------|-------------|
| `mongo_data` (named volume) | MongoDB data (`/data/db`) — the entrypoint self-chowns it on init |
| `meshcentral_data` (named volume) | MeshCentral data: generated certificates, the auto-persisted session secret, and database adjuncts (`/opt/meshcentral/meshcentral-data`) |

Both are **named volumes**, not host bind mounts: wdm does not pre-create host
bind directories. The wdm-rendered `config.json` is bind-mounted read-only on
top of the `meshcentral_data` mount path. wdm never runs
`docker compose down -v`, so removing the stack **preserves** both volumes —
they are listed in the removal summary (as `wdm-meshcentral_mongo_data` and
`wdm-meshcentral_meshcentral_data`).

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL` (mongodb adds 5 init caps; meshcentral adds none) | No NET/SYS caps anywhere |
| Privileges | `security_opt: no-new-privileges` on all containers | Setuid binaries cannot gain caps |
| IPC | `ipc: private` on all containers | Isolated SysV/POSIX IPC namespace |
| Process budget | `pids` limits per container | Caps fork sprawl |
| Memory / CPU | Per-container limits | One service can't starve the other |
| Two-network split | `meshcentral-db` created with `--internal` | MongoDB has no internet egress |
| Port exposure | `127.0.0.1:4430` only | Only the reverse proxy can reach MeshCentral |
| Config mount | `config.json` mounted `:ro` | A hypothetical RCE cannot rewrite its own config |
| Healthchecks | `mongosh` ping (mongodb) / `wget /health.ashx` (meshcentral) | No credentials on the command line |

> **Container-root, zero capabilities.** The MeshCentral image is Alpine-based
> and ships no usable non-root user — Node and the self-signed certificate
> generation both run as root. There is therefore no `user:` directive to set.
> `cap_drop: ALL` with no `cap_add` plus `no-new-privileges` contain the root
> process: it holds no Linux capabilities and cannot acquire any. Binding
> container port 443 and generating certs need no added capability under the
> image's root process (probe-verified).

> **First boot is slow.** MeshCentral generates a self-signed certificate and
> initializes the MongoDB collections on first start. The compose healthcheck
> uses a 60s `start_period` and probes `http://127.0.0.1:443/health.ashx`
> (returns 200 `ok` once ready).

## Support the Project

- ⭐ [Star on GitHub](https://github.com/Ylianst/MeshCentral)
- 💬 [Reddit community](https://www.reddit.com/r/MeshCentral/)
- 📖 [Documentation](https://ylianst.github.io/MeshCentral/)

## License

MeshCentral is released under the [Apache License 2.0](https://github.com/Ylianst/MeshCentral/blob/master/LICENSE).
