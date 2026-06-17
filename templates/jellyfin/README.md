# Jellyfin

<p align="center">
  <img src="https://raw.githubusercontent.com/jellyfin/jellyfin-ux/master/branding/SVG/banner-logo-solid.svg" alt="Jellyfin Logo" width="400">
</p>

<p align="center">
  <a href="https://jellyfin.org/">Website</a> •
  <a href="https://jellyfin.org/docs/">Documentation</a> •
  <a href="https://github.com/jellyfin/jellyfin">GitHub</a> •
  <a href="https://discord.com/invite/jellyfin-772232779534172171">Discord</a>
</p>

---

[Jellyfin](https://jellyfin.org/) is a free and open-source media server. Manage and stream your movies, TV shows, music, and photos with no tracking, no central servers, and complete privacy.

## Features

- **Completely Free** — No subscriptions, no paywalls
- **Multi-Platform** — Clients for web, mobile, TV, and desktop
- **Live TV & DVR** — Watch and record live television
- **Rich Metadata** — Automatic media information and artwork
- **User Profiles** — Multiple users with parental controls
- **Hardware Acceleration** — GPU transcoding support
- **Plugins** — Extend functionality with community plugins

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik)
- Media files accessible to the container

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `jellyfin` network, and runs `docker compose up -d` at install time. `PUID` and `PGID` are auto-resolved from `os.Getuid()` / `os.Getgid()`, so jellyfin's s6-overlay drops privileges to the host user that owns the stack directory. `MEDIA_PATH`, `MOVIES_PATH`, and `SHOWS_PATH` are collected at install time; wdm validates each path is absolute, exists, and lives outside the stack dir (symlinks resolved before that check), then mounts them read-only into the container. See the wdm documentation for the CLI surface; do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

After install, open Jellyfin at `http://127.0.0.1:8096` (or via your reverse proxy), follow the setup wizard, add your media libraries pointing at `/data/media`, `/data/movies`, and `/data/shows` inside the container, and create user accounts. The reference content below describes the rendered stack (configuration knobs, security baseline, data layout) for troubleshooting and catalog-template work.

## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `PUID`   | Host UID that owns `/config` and media | `1000` |
| `PGID`   | Host GID that owns `/config` and media | `1000` |
| `TZ`     | Container timezone | `Europe/Bratislava` |
| `UMASK`  | File-creation mask (`022` = files 644 / dirs 755) | `022` |

### Reverse Proxy (Caddy)

```
jellyfin.example.com {
    reverse_proxy http://localhost:8096
}
```

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 8096 | HTTP | Web interface |

## Data Persistence

| Path | Description |
|------|-------------|
| `./config` | Jellyfin configuration |
| `/data/media` | Media files (read-only) |
| `/data/movies` | Movies (read-only) |
| `/data/shows` | TV Shows (read-only) |

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL` + 5 minimum caps (CHOWN/SETUID/SETGID/DAC_OVERRIDE/FOWNER) | Only what s6-overlay needs for PUID/PGID swap; no NET/SYS caps |
| Privileges | `security_opt: no-new-privileges` | Setuid binaries cannot gain caps |
| IPC | `ipc: private` | Isolated SysV/POSIX IPC namespace |
| Process budget | `pids: 500` | Caps ffmpeg thread sprawl; fork-bomb resistance |
| Memory / CPU | 4 GiB / 4 CPUs limit | Won't starve other stacks during transcodes |
| Port exposure | `127.0.0.1:8096:8096` | Only the reverse proxy can reach jellyfin |
| Media volumes | `:ro` bind mounts | A hypothetical jellyfin RCE can't tamper with your library |
| Healthcheck | `curl /health` (unauthenticated endpoint) | No credentials on the command line |
| Ephemeral writes | `tmpfs` for `/tmp` (512 MiB) | ffmpeg scratch stays in RAM, never hits disk |

## Hardware Acceleration

GPU transcoding stays compatible with the hardening above, but enabling it
requires extending the catalog template (`docker-compose.yml.tmpl` in the
wdm source tree) — wdm regenerates the rendered `docker-compose.yml` on
update, so user-side edits to the rendered file are overwritten. Uncomment
the `devices` + `group_add` block in the template:

```yaml
devices:
  - /dev/dri/renderD128:/dev/dri/renderD128
  - /dev/dri/card0:/dev/dri/card0
group_add:
  - "104"  # Replace with your host's `render` group GID
```

Find your `render` GID with:

```bash
getent group render | cut -d: -f3
```

For NVIDIA NVENC, use the dedicated runtime instead (see the commented
block in `docker-compose.yml.tmpl`). Then enable hardware acceleration in
Jellyfin's Dashboard → Playback settings.

## Client Apps

Jellyfin has official and third-party apps for:

- **Web** — Built-in web interface
- **Android** — Jellyfin for Android
- **iOS** — Jellyfin Mobile, Swiftfin
- **TV** — Android TV, Fire TV, Roku, webOS, Tizen
- **Desktop** — Jellyfin Media Player

## Support the Project

- ⭐ [Star on GitHub](https://github.com/jellyfin/jellyfin)
- 💵 [Donate via OpenCollective](https://opencollective.com/jellyfin)
- 💬 [Join Discord](https://discord.com/invite/jellyfin-772232779534172171)
- 🐛 [Report Issues](https://github.com/jellyfin/jellyfin/issues)

## Docker Image

This template uses the [LinuxServer.io](https://docs.linuxserver.io/images/docker-jellyfin/) image for reliable updates and consistent configuration.

## License

Jellyfin is released under the [GPL-2.0 License](https://github.com/jellyfin/jellyfin/blob/master/LICENSE).