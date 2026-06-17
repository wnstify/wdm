# Navidrome

<p align="center">
  <img src="https://www.navidrome.org/images/navidrome-logo-180x180.png" alt="Navidrome Logo" width="150">
</p>

<p align="center">
  <a href="https://www.navidrome.org/">Website</a> •
  <a href="https://www.navidrome.org/docs/">Documentation</a> •
  <a href="https://github.com/navidrome/navidrome">GitHub</a> •
  <a href="https://discord.gg/xh7j7yF">Discord</a>
</p>

---

[Navidrome](https://www.navidrome.org/) is a free and open-source music server and streamer. Give yourself access to your entire music collection from any browser or mobile device, with a fast and modern web UI and broad Subsonic-API client support.

## Features

- **Stream Anywhere** — Web UI plus any Subsonic/OpenSubsonic client
- **Handles Large Libraries** — Efficient indexing of tens of thousands of tracks
- **Transcoding On-the-Fly** — Adapts bitrate to the network and device
- **Multi-User** — Separate accounts, playlists, and play history
- **No Lock-In** — Reads tags from your existing files; no library reshuffling
- **Last.fm & Spotify** — Optional scrobbling and artist artwork
- **Low Footprint** — Single Go binary, idles around ~20 MiB of RAM

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik)
- A music library directory accessible to the container

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `navidrome` network, and runs `docker compose up -d` at install time. `TZ` defaults to the host timezone. `MUSIC_PATH` is collected at install time; wdm validates the path is absolute, exists, and lives outside the stack dir (symlinks resolved before that check), then mounts it read-only into the container at `/music`. See the wdm documentation for the CLI surface; do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

After install, open Navidrome at `http://127.0.0.1:4533` (or via your reverse proxy) and create the first admin account on the welcome screen. Navidrome scans `/music` on startup and then on the `ND_SCANSCHEDULE` interval (default hourly). The reference content below describes the rendered stack (configuration knobs, security baseline, data layout) for troubleshooting and catalog-template work.

## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `TZ` | Container timezone | `Europe/Bratislava` |
| `ND_MUSICFOLDER` | Music library mount point inside the container (fixed) | `/music` |
| `ND_DATAFOLDER` | Data dir inside the container (fixed) | `/data` |
| `ND_LOGLEVEL` | Log verbosity (`error`/`warn`/`info`/`debug`/`trace`) | `info` |
| `ND_SCANSCHEDULE` | Library rescan interval (`0` disables periodic scans) | `1h` |
| `ND_SESSIONTIMEOUT` | Web session lifetime | `24h` |

`ND_LOGLEVEL`, `ND_SCANSCHEDULE`, and `ND_SESSIONTIMEOUT` are catalog-fixed
literals in `.env.tmpl`, not wdm placeholders. Optional integrations
(`ND_BASEURL`, `ND_LASTFM_*`, `ND_SPOTIFY_*`) ship commented out in
`.env.tmpl`; `env_file` picks them up if you uncomment them in the wdm
template (the rendered `.env` is overwritten on update). The full list of
configuration options is in the [Navidrome docs](https://www.navidrome.org/docs/usage/configuration-options/).

### Reverse Proxy (Caddy)

```
music.example.com {
    reverse_proxy http://localhost:4533
}
```

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 4533 | HTTP | Web interface & Subsonic API |

## Data Persistence

| Path | Description |
|------|-------------|
| `navidrome_data` (named volume) | SQLite database, cache, and downloaded artwork (`/data`) |
| `/music` (read-only bind) | Your music library — `MUSIC_PATH` on the host |

`navidrome_data` is a **named volume**, not a host bind mount. The
`deluan/navidrome` image is distroless: it ships no non-root user and no
shell or `chown` to drop privileges with, so the process runs as
container-root. A named volume keeps the database and cache self-contained
rather than scattering a root-owned directory across the host. wdm never
runs `docker compose down -v`, so removing the stack **preserves**
`navidrome_data` (listed in the removal summary as `wdm-navidrome_navidrome_data`).
The music library lives entirely outside the stack and is mounted read-only.

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL`, zero added | Container holds no Linux capabilities (probe-verified healthy) |
| User | Runs as container-root | The distroless image provides no non-root user; `cap_drop: ALL` + no-new-privileges contain the root process so it has no capabilities and cannot acquire any |
| Privileges | `security_opt: no-new-privileges` | Setuid binaries cannot gain caps |
| IPC | `ipc: private` | Isolated SysV/POSIX IPC namespace |
| Process budget | `pids: 200` | Caps fork sprawl; fork-bomb resistance |
| Memory / CPU | 256 MiB / 0.5 CPU recommended | Won't starve other stacks (idle is ~20 MiB) |
| Port exposure | `127.0.0.1:4533:4533` | Only the reverse proxy can reach navidrome |
| Music library | `:ro` bind mount | A hypothetical navidrome RCE can't tamper with your files |
| Healthcheck | `wget /ping` (unauthenticated endpoint) | No credentials on the command line |
| Ephemeral writes | `tmpfs` for `/tmp` (128 MiB) | Transcode scratch stays in RAM, never hits disk |

## Client Apps

Navidrome speaks the Subsonic / OpenSubsonic API, so a large ecosystem of
clients works out of the box:

- **Web** — Built-in web interface
- **Android** — Symfonium, Tempo, DSub, Substreamer
- **iOS** — play:Sub, Amperfy, substreamer
- **Desktop** — Sonixd, Supersonic, Feishin

Point any Subsonic client at your Navidrome URL and log in with your account.

## Support the Project

- ⭐ [Star on GitHub](https://github.com/navidrome/navidrome)
- 💵 [Sponsor on GitHub](https://github.com/sponsors/deluan)
- 💬 [Join Discord](https://discord.gg/xh7j7yF)
- 🐛 [Report Issues](https://github.com/navidrome/navidrome/issues)

## Docker Image

This template uses [`deluan/navidrome:0.61.2`](https://hub.docker.com/r/deluan/navidrome),
the upstream-official image maintained directly by the Navidrome project.

## License

Navidrome is released under the [GPL-3.0 License](https://github.com/navidrome/navidrome/blob/master/LICENSE).
