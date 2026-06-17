# qBittorrent

<p align="center">
  <a href="https://www.qbittorrent.org/">Website</a> •
  <a href="https://github.com/qbittorrent/qBittorrent/wiki">Documentation</a> •
  <a href="https://github.com/qbittorrent/qBittorrent">GitHub</a>
</p>

---

[qBittorrent](https://www.qbittorrent.org/) is a free and open-source BitTorrent client with a built-in web UI. It downloads and seeds torrents, supports a search plugin system and RSS auto-downloading, and is managed entirely from the browser — no desktop app required. This template ships the upstream-official `qbittorrent-nox` (headless) image.

## Features

- **Web UI** — Manage torrents, queues, and settings from any browser
- **No Ads, No Tracking** — Free and open source, funded by the community
- **RSS Auto-Downloading** — Subscribe to feeds and grab matching torrents
- **Search Plugins** — Search across indexers from inside the UI
- **Sequential Downloading** — Stream-friendly piece ordering
- **Low Footprint** — Headless `-nox` build idles around ~15 MiB of RAM

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik) for the web UI
- A downloads directory accessible to the container
- A router/firewall port-forward for the BitTorrent listen port (6881)

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `qbittorrent` network, and runs `docker compose up -d` at install time. `TZ` defaults to the host timezone. `DOWNLOADS_PATH` is collected at install time; wdm validates the path is absolute, exists, and lives outside the stack dir (symlinks resolved before that check), then mounts it read-write into the container at `/downloads`. See the wdm documentation for the CLI surface; do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

After install, open qBittorrent at `http://127.0.0.1:8090` (or via your reverse proxy). **On first run the image prints a temporary admin password to the container logs** — read it with `docker logs qbittorrent | grep "temporary password"`, sign in as `admin`, then **set your own password in the WebUI (Tools → Options → Web UI)**. The default password is single-use noise; change it before exposing the UI through a reverse proxy. The reference content below describes the rendered stack (configuration knobs, security baseline, data layout) for troubleshooting and catalog-template work.

## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `PUID` / `PGID` | Host user/group the process drops to (wdm built-in) | host user |
| `TZ` | Container timezone | `Europe/Bratislava` |
| `QBT_LEGAL_NOTICE` | Accepts the upstream legal notice non-interactively (fixed) | `confirm` |
| `QBT_WEBUI_PORT` | WebUI port inside the container (fixed) | `8080` |
| `QBT_TORRENTING_PORT` | BitTorrent listen port inside the container (fixed) | `6881` |

`QBT_LEGAL_NOTICE`, `QBT_WEBUI_PORT`, and `QBT_TORRENTING_PORT` are
catalog-fixed literals in `.env.tmpl`, not wdm placeholders. `QBT_LEGAL_NOTICE`
must stay `confirm` — the image refuses to start otherwise. The full
configuration surface lives in the WebUI under **Tools → Options**.

### Reverse Proxy (Caddy)

```
qbittorrent.example.com {
    reverse_proxy http://localhost:8090
}
```

Front only the WebUI. The BitTorrent listen port is a raw peer protocol, not
HTTP — do not proxy it; forward it at the router/firewall instead (see Ports).

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 8090 | WebUI (HTTP) | Web interface — bound to `127.0.0.1` |
| 6881/tcp | BitTorrent | Inbound peer connections — **public, all interfaces** |
| 6881/udp | BitTorrent (DHT/µTP) | DHT and µTP peer traffic — **public, all interfaces** |

The WebUI stays localhost-only and is reached through your reverse proxy. The
BitTorrent listen port (6881, both TCP and UDP) binds all interfaces on purpose
so the swarm can open connections back to this client — that is the only public
surface, declared `public:true` in the signed catalog (PRD §11.1). **Forward
6881/tcp and 6881/udp to this host in your router and firewall** so inbound
peers and DHT reach you; without the forward, downloads still work but
connectivity and speeds are degraded.

## Data Persistence

| Path | Description |
|------|-------------|
| `qbittorrent_config` (named volume) | Settings, session state, and the DHT checkpoint (`/config`) |
| `/downloads` (read-write bind) | Your downloads — `DOWNLOADS_PATH` on the host |

`qbittorrent_config` is a **named volume**, not a host bind mount. The image
entrypoint starts as container-root, chowns the fresh volume to `PUID:PGID`,
then drops the `qbittorrent-nox` process to that uid. A named volume is seeded
writable from the image, so the first run never has to pre-create a host
directory. wdm never runs `docker compose down -v`, so removing the stack
**preserves** `qbittorrent_config` (listed in the removal summary as
`wdm-qbittorrent_qbittorrent_config`), and your settings and torrent session
survive a remove. The downloads library lives entirely outside the stack and is
mounted read-write so qBittorrent can save files.

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL`, adds `CHOWN`, `SETUID`, `SETGID`, `DAC_OVERRIDE` | Only the caps the entrypoint needs to chown `/config` and drop privileges; no NET/SYS caps |
| User | Entrypoint starts root, drops to uid 1000 | Starts root to chown the fresh volume, then runs the process unprivileged (probe-confirmed via `docker top`); no `user:` override, which would EACCES the first run |
| Privileges | `security_opt: no-new-privileges` | Setuid binaries cannot gain caps after the drop |
| IPC | `ipc: private` | Isolated SysV/POSIX IPC namespace |
| Process budget | `pids: 300` | Caps fork sprawl; fork-bomb resistance |
| Memory / CPU | 512 MiB / 0.5 CPU recommended | Won't starve other stacks (idle is ~15 MiB; the libtorrent piece cache grows with active torrents) |
| WebUI exposure | `127.0.0.1:8090:8080` | Only the reverse proxy can reach the admin UI |
| BitTorrent exposure | `6881:6881/tcp` + `6881:6881/udp` (all interfaces) | Intentional public peer surface (PRD §11.1); the only port bound off localhost |
| Downloads | read-write bind mount | qBittorrent writes downloads to your host directory |
| Healthcheck | `wget` WebUI root (no creds on cmdline) | Built-in busybox probe only |
| Shutdown | `stop_grace_period: 30m` | Lets qBittorrent flush DHT state and checkpoint torrents; a shorter window corrupts in-progress downloads |

> **First-run password.** The image generates a one-time admin password and
> prints it to the container logs. Read it with
> `docker logs qbittorrent | grep "temporary password"`, then set your own in
> the WebUI before fronting it with a reverse proxy.

> **Why no `user:` directive?** The image's default user is root, and the
> entrypoint must start as root to chown the fresh `/config` volume before
> dropping to uid 1000. Forcing `user: 1000` against a fresh named volume
> EACCESes the first `mkdir` (`can't create directory '/config/qBittorrent/'`)
> — verified in testing. Running container-root with `cap_drop: ALL` plus
> the four entrypoint caps and `no-new-privileges` is the only configuration
> that comes up clean while still containing the dropped process.

## Support the Project

- ⭐ [Star on GitHub](https://github.com/qbittorrent/qBittorrent)
- 🐛 [Report Issues](https://github.com/qbittorrent/qBittorrent/issues)
- 📖 [Wiki](https://github.com/qbittorrent/qBittorrent/wiki)

## Docker Image

This template uses [`qbittorrentofficial/qbittorrent-nox:5.2.1-1`](https://hub.docker.com/r/qbittorrentofficial/qbittorrent-nox),
the upstream-official headless image maintained by the qBittorrent project
(libtorrent 1.2 build).

## License

qBittorrent is released under the [GPL-2.0 License](https://github.com/qbittorrent/qBittorrent/blob/master/COPYING).
