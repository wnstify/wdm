# Open WebUI

<p align="center">
  <img src="https://raw.githubusercontent.com/open-webui/open-webui/main/static/favicon.png" alt="Open WebUI Logo" width="120">
</p>

<p align="center">
  <a href="https://openwebui.com/">Website</a> •
  <a href="https://docs.openwebui.com/">Documentation</a> •
  <a href="https://github.com/open-webui/open-webui">GitHub</a> •
  <a href="https://discord.gg/5rJgQTnV4s">Discord</a>
</p>

---

[Open WebUI](https://openwebui.com/) is an extensible, self-hosted web interface for large language models. It runs entirely offline and connects to a local [Ollama](https://ollama.com/) instance or any OpenAI-compatible API, giving you a private chat UI with multi-user support, RAG, and a model playground.

## Features

- **Provider-Agnostic** — Connect to Ollama or any OpenAI-compatible API
- **Multi-User** — Account management with role-based access
- **Retrieval (RAG)** — Chat over your own documents and web pages
- **Markdown & Code** — Rich rendering with syntax highlighting
- **Model Playground** — Compare models and tune parameters
- **Fully Self-Hosted** — Your conversations never leave your server

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik)
- A model provider — a local Ollama instance **or** an external model API key (see below)

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `openwebui` network, generates `WEBUI_SECRET_KEY` via `crypto/rand`, and runs `docker compose up -d` at install time. `WEBUI_SECRET_KEY` is marked `regenerable=false`: rotating it logs every user out (no data loss), so wdm reuses the existing value via `state.ReadStackEnv` on every update. See the wdm documentation for the CLI surface; do not edit the rendered `docker-compose.yml` by hand, since wdm regenerates it on update.

First boot downloads a small sentence-transformers embedding model over the network before the app reports healthy — the healthcheck `start_period` is set generously (90s) to allow for it. After install, open Open WebUI at `http://127.0.0.1:8071` (or via your reverse proxy) and **create the first account, which becomes the admin**. The reference content below describes the rendered stack (configuration knobs, security baseline, data layout) for troubleshooting and catalog-template work.

## Connecting a model provider

Open WebUI is a **front-end only**: it boots and serves the UI without a provider, but cannot chat until one is connected. The `.env` ships with commented hints picked up automatically via `env_file` — uncomment one and reinstall/update, or set the provider from the in-app **Admin → Settings → Connections** panel:

- **Local Ollama** — run Ollama on the host and set
  `OLLAMA_BASE_URL=http://host.docker.internal:11434`.
- **External OpenAI-compatible API** — set `OPENAI_API_KEY` (and, for a
  non-OpenAI endpoint, the matching base URL in the Admin panel).

## Configuration

### Environment Variables

| Variable | Description | Required |
|---|---|---|
| `WEBUI_SECRET_KEY` | Session-cookie signing key (`regenerable=false`) | Yes |
| `WEBUI_AUTH` | Require login (catalog-fixed `true`) | Yes |
| `TZ` | Container timezone | No (default: host timezone) |
| `OLLAMA_BASE_URL` | Local Ollama endpoint (optional) | No |
| `OPENAI_API_KEY` | External model API key (optional) | No |

### Reverse Proxy (Caddy)

```
chat.example.com {
    reverse_proxy http://localhost:8071
}
```

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 8071 | HTTP | Web interface (bound to `127.0.0.1`) |

## Data Persistence

| Storage | Description |
|------|-------------|
| `openwebui_data` (named volume) | SQLite DB, settings, chats, and the bundled vector store (`/app/backend/data`) |

`openwebui_data` is a **named volume**, not a host bind mount. The upstream image
runs as container-root and stores its SQLite database under
`/app/backend/data`; a host bind mount's source is auto-created root-owned and
would leave root-owned files on the host, while a named volume is initialized by
Docker from the image and stays self-contained (mirroring n8n's `n8n_storage`).
wdm never runs `docker compose down -v`, so removing the stack **preserves**
`openwebui_data` (listed in the removal summary as `wdm-openwebui_openwebui_data`),
and your chats and settings survive a remove.

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL` (no caps added) | No NET/SYS caps; minimal kernel surface |
| User | Runs as container-root | The image provides no non-root user, and a forced non-root UID breaks its SQLite DB; all caps dropped + no-new-privileges keep the blast radius small |
| Privileges | `security_opt: no-new-privileges` | Setuid binaries cannot gain caps |
| IPC | `ipc: private` | Isolated SysV/POSIX IPC namespace |
| Process budget | `pids: 300` | Caps fork sprawl; fork-bomb resistance |
| Memory / CPU | 2 GiB / 1.0 CPU recommended (4 GiB / 4.0 CPU ceiling) | Won't starve other stacks |
| Port exposure | `127.0.0.1:8071:8080` | Only the reverse proxy can reach Open WebUI |
| Data volume | Named volume `openwebui_data` | No root-owned files on the host bind path |
| Healthcheck | `curl /health` (unauthenticated endpoint) | No credentials on the command line |
| Ephemeral writes | `tmpfs` for `/tmp` (256 MiB) | Scratch stays in RAM, never hits disk |

> **First boot is slow.** The container downloads a sentence-transformers
> embedding model (~30 files) before `/health` returns 200. The generous
> `start_period` (90s) keeps Docker from marking the container unhealthy while
> that download runs.

## Support the Project

- ⭐ [Star on GitHub](https://github.com/open-webui/open-webui)
- 💬 [Join Discord](https://discord.gg/5rJgQTnV4s)
- 📖 [Documentation](https://docs.openwebui.com/)
- 🐛 [Report Issues](https://github.com/open-webui/open-webui/issues)

## License

Open WebUI is released under the [Open WebUI License](https://github.com/open-webui/open-webui/blob/main/LICENSE) (BSD-3-Clause-based, with branding terms).
