# Stoat

<p align="center">
  <a href="https://stoat.chat/">Website</a> •
  <a href="https://github.com/stoatchat">GitHub</a>
</p>

---

[Stoat](https://stoat.chat/) is an open-source, self-hosted chat platform — the
community continuation of Revolt. It offers servers, channels, direct messages,
roles, voice/video, and a modern web client, as a self-hosted alternative to
Discord.

## Features

- **Servers, Channels, DMs** — Topic spaces, text channels, and direct messages
- **Roles & Permissions** — Per-server role hierarchy and channel overrides
- **Voice & Video** — LiveKit-backed WebRTC voice and video rooms
- **File Sharing** — Attachments and avatars stored in an in-stack S3 object store
- **Self-Hosted** — Full control over your data and the chat plane

## Architecture

Stoat is a large multi-service deployment. wdm renders and manages:

- **database** — MongoDB backend
- **redis** — cache
- **rabbit** — RabbitMQ event bus
- **garage** — Garage S3-compatible object store (file attachments)
- **api / events / autumn / january / gifbox / crond / pushd / voice-ingress** —
  eight Rust microservices
- **livekit** — voice/video SFU (the only public ports)
- **web** — the web frontend
- **caddy** — in-stack router (localhost only; front with a reverse proxy)
- **mongo-init / garage-init** — one-shot bootstrap containers

Five segmented Docker networks isolate the front, app, data, rabbit, and voice
paths; only the front network reaches the internet.

## Prerequisites

- Docker and Docker Compose
- A real fully qualified domain name (Stoat crash-loops on a dotless host)
- A reverse proxy (Caddy, Nginx, Traefik) terminating TLS in front of the stack
- Firewall access for the public LiveKit voice ports (see below) if you want
  voice/video

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl`,
`.env.tmpl`, and five **config-generation** artifacts (`secrets.env`,
`garage.toml`, `livekit.yml`, `.env.web`, `Revolt.toml`) from the placeholder
map, copies two static sidecars (`Caddyfile`, `init-scripts/init-garage.sh`),
pre-creates the five Docker networks (four `--internal`), and runs `docker
compose up -d`. wdm is the sole config generator — it reproduces the upstream
`generate-config.sh` output and never invokes that script. Do not edit the
rendered files by hand; wdm regenerates them on update (regenerable=false secrets
are read back from the existing files).

After install, front the in-stack Caddy with your reverse proxy (point it at
`http://127.0.0.1:8880`) and open Stoat through it. Create the first account via
the sign-up flow — the first account registered becomes the instance owner.
Registration is open by default (`invite_only=false`).

## Public voice ports

LiveKit publishes the only public ports — the WebRTC TCP signal (`7881/tcp`) and
the UDP media range (`50000-50100/udp`) — so remote voice/video peers connect
directly. They bind all interfaces by design; open/forward them at your firewall
for voice and **do not** reverse-proxy them. Every other surface stays on
localhost behind Caddy.

## V1 skips

Text chat and file/avatar/image uploads work out of the box. These features
need operator follow-up:

- **Web push (VAPID)** — pushd starts with browser push disabled; wdm cannot
  mint the EC P-256 keypair.
- **LiveKit voice/video media** — the signalling and SFU are wired, but media
  needs the public ports reachable and a working external-IP path.
- **GIF search (gifbox)** — runs but needs upstream configuration to surface
  results.

## Security baseline

- `cap_drop: ALL` on every container, re-adding only what each one provably
  needs. The data services (`database`, `redis`, `rabbit`) use the gosu-drop
  posture: the entrypoint starts as root, chowns its named volume, and drops to
  the image uid, which needs the five init / privilege-drop caps. `garage` is the
  lone zero-cap data service. `caddy` adds only `NET_BIND_SERVICE`. The Rust
  services, `web`, `livekit`, and both init containers run with zero added caps.
- `security_opt: no-new-privileges`, `ipc: private`, and tmpfs for ephemeral
  writes on every container.
- Named volumes for all persistent data (never `docker compose down -v`).
- 127.0.0.1-only host binding except the public LiveKit voice ports.

## Data layout

Persistent state lives in named volumes preserved on remove: `mongo_data`,
`redis_data`, `rabbit_data`, `garage_meta`, and `garage_data`. Back these up
before maintenance.
