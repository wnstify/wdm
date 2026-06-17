# Syncthing

<p align="center">
  <img src="https://raw.githubusercontent.com/syncthing/syncthing/main/assets/logo-128.png" alt="Syncthing Logo" width="128">
</p>

<p align="center">
  <a href="https://syncthing.net/">Website</a> •
  <a href="https://docs.syncthing.net/">Documentation</a> •
  <a href="https://github.com/syncthing/syncthing">GitHub</a> •
  <a href="https://forum.syncthing.net/">Forum</a>
</p>

---

[Syncthing](https://syncthing.net/) is a free, open-source, continuous file-synchronization program. It syncs files between two or more devices over an encrypted peer-to-peer connection — your data never passes through a third-party server. There is no central account and no cloud: each device holds its own identity, and you choose which folders sync to which peers.

## Features

- **Peer-to-Peer** — Files sync directly between your devices; no cloud middleman
- **End-to-End Encrypted** — Every transfer uses TLS between authenticated devices
- **Continuous Sync** — Changes propagate automatically as files are added or edited
- **Versioning** — Optional file versioning recovers earlier copies after a change
- **Cross-Platform** — Linux, macOS, Windows, BSD, Android, and more
- **No Lock-In** — Reads your existing directory layout; no proprietary container
- **Low Footprint** — Single Go binary, idles around ~25 MiB of RAM

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik) for the web UI
- A directory to sync, accessible to the container
- Inbound access to TCP/UDP port 22000 for direct peer connections

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `syncthing` network, and runs `docker compose up -d` at install time. `TZ` defaults to the host timezone. `SYNC_PATH` is collected at install time; wdm validates the path is absolute, exists, and lives outside the stack dir (symlinks resolved before that check), then mounts it read-write into the container at `/data`. See the wdm documentation for the CLI surface; do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

On first boot Syncthing generates a device ID and a self-signed TLS certificate, then serves the web UI on port 8384. Open it at `http://127.0.0.1:8384` (or via your reverse proxy). **The GUI has no authentication out of the box** — set a username and password under **Actions → Settings → GUI** before exposing it through a reverse proxy. Add a remote device by exchanging device IDs, then share a folder with it; the BEP sync port (22000) is published publicly so peers can connect directly. The reference content below describes the rendered stack (configuration knobs, security baseline, data layout) for troubleshooting and catalog-template work.

## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `PUID` | Host user id Syncthing drops to (wdm-resolved, fixed) | host user |
| `PGID` | Host group id Syncthing drops to (wdm-resolved, fixed) | host group |
| `TZ` | Container timezone | `Europe/Bratislava` |
| `STGUIADDRESS` | GUI bind address inside the container (fixed) | `0.0.0.0:8384` |

`STGUIADDRESS` is a catalog-fixed literal in `.env.tmpl`, not a wdm placeholder.
It binds the web UI on all *container* interfaces so the host-side
`127.0.0.1:8384` mapping can reach it; the GUI is still localhost-only on the
host. `PUID`/`PGID` are wdm built-in template vars (`.UID`/`.GID`), never
user-supplied. The full list of configuration options is in the
[Syncthing docs](https://docs.syncthing.net/users/config.html).

### Reverse Proxy (Caddy)

```
syncthing.example.com {
    reverse_proxy http://localhost:8384
}
```

Only proxy the GUI (8384). The BEP sync port (22000) is a raw protocol port,
not HTTP — forward it at the firewall, do not put it behind the reverse proxy.

## Ports

| Port | Protocol | Exposure | Description |
|------|----------|----------|-------------|
| 8384 | TCP | `127.0.0.1` only | Web UI & REST API |
| 22000 | TCP | **public** (all interfaces) | BEP sync protocol — peer connections |
| 22000 | UDP | **public** (all interfaces) | BEP sync protocol (QUIC transport) |

Port 22000 is published on all interfaces by design so remote peers can reach
this instance directly; it is declared `public: true` in the signed catalog
(PRD §11.1 public-port exception). Local-discovery broadcast (21027/udp) is
LAN-only and is not host-published — direct peer connections use 22000.

## Data Persistence

| Path | Description |
|------|-------------|
| `syncthing_config` (named volume) | Device identity, TLS cert, config, and SQLite index (`/var/syncthing`) |
| `/data` (read-write bind) | The synced folder — `SYNC_PATH` on the host |

`syncthing_config` is a **named volume**, not a host bind mount. The image
runs as container-root and chowns its home directory to `PUID:PGID` on boot,
so a named volume keeps the device identity and index self-contained rather
than scattering a chowned tree across the host. wdm never runs
`docker compose down -v`, so removing the stack **preserves**
`syncthing_config` (listed in the removal summary as
`wdm-syncthing_syncthing_config`) — the device ID, and therefore the peer
relationships, survive a remove. The synced folder lives entirely outside the
stack and is mounted read-write so Syncthing can apply changes received from
peers.

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL` + `CHOWN`, `SETUID`, `SETGID`, `DAC_OVERRIDE` | Only the minimum caps the entrypoint needs to chown the home dir and drop privileges; zero caps exits on boot |
| User | Container-root that drops to uid 1000 | The image starts as root and the entrypoint switches to `PUID:PGID`; the long-running process runs as the host user (`docker top`-verified) |
| Privileges | `security_opt: no-new-privileges` | Setuid binaries cannot gain caps |
| IPC | `ipc: private` | Isolated SysV/POSIX IPC namespace |
| Process budget | `pids: 200` | Caps fork sprawl; fork-bomb resistance |
| Memory / CPU | 256 MiB / 0.5 CPU recommended | Won't starve other stacks (idle is ~25 MiB) |
| GUI exposure | `127.0.0.1:8384:8384` | The no-auth-on-first-boot web UI is localhost-only — set a GUI password before exposing it |
| Sync port | `22000` tcp+udp public | The one deliberately public surface so remote peers can connect directly (PRD §11.1) |
| Healthcheck | `wget /rest/noauth/health` (unauthenticated endpoint) | No credentials on the command line |

**Set a GUI password before exposing the web UI.** Syncthing's GUI has no
authentication on first boot. Anyone who can reach port 8384 can change your
folder and device configuration, so the GUI binds `127.0.0.1` only; set a
username and password under **Actions → Settings → GUI** before putting it
behind a reverse proxy.

## Connecting Devices

1. Open the web UI and copy this instance's **Device ID** (Actions → Show ID).
2. On the remote device, add this Device ID; on this instance, add the remote
   one. Each side must accept the other.
3. Share a folder with the connected device and accept the share on the peer.

Direct connections use the public BEP port (22000). If both devices are behind
NAT and cannot connect directly, Syncthing falls back to the public relay
network automatically.

## Support the Project

- ⭐ [Star on GitHub](https://github.com/syncthing/syncthing)
- 💵 [Donate](https://syncthing.net/donations/)
- 💬 [Community Forum](https://forum.syncthing.net/)
- 🐛 [Report Issues](https://github.com/syncthing/syncthing/issues)

## Docker Image

This template uses [`syncthing/syncthing:2.1.1`](https://hub.docker.com/r/syncthing/syncthing),
the upstream-official image maintained directly by the Syncthing project.

## License

Syncthing is released under the [MPL-2.0 License](https://github.com/syncthing/syncthing/blob/main/LICENSE).
