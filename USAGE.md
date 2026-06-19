# wdm CLI Reference

`wdm` is a terminal application for installing, updating, and checking a curated set of Docker Compose self-hosting templates. It provides an interactive TUI and a scriptable CLI over the same engine.

Run `wdm` with no arguments in an interactive terminal to launch the TUI. When run in a pipe or a script, `wdm` prints this CLI help instead of starting the interactive program.

## Global flags

Every command accepts these flags:

- `--json` — emit machine-readable output via the `wdm.v1` JSON envelope instead of human-readable text.
- `--version` — print the `wdm` version and exit.
- `--help` — print help for `wdm` or any subcommand.

Run `wdm <command> --help` for a command's full flag set.

## Commands

### Apps

Manage the Docker Compose stacks that `wdm` installs under `~/docker/<app>/`.

| Command | Description |
|---|---|
| `wdm apps list` | List managed Docker Compose stacks with their live runtime state. |
| `wdm apps install <app-id>` | Install and start a curated stack. |
| `wdm apps status <app-id>` | Show the runtime status of a managed stack. |
| `wdm apps logs <app-id>` | Stream redacted logs from a stack. |
| `wdm apps update <app-id>` | Update a stack to the current catalog version. |
| `wdm apps restart <app-id>` | Restart a stack's containers in place. |
| `wdm apps stop-all` | Stop every running managed stack at once, preserving all data. |
| `wdm apps remove <app-id>` | Remove a stack, keeping its files and volumes. |
| `wdm apps delete <app-id>` | Permanently delete a stack's files and directory. |
| `wdm apps validate <app-id>` | Validate a stack's Docker Compose configuration. |
| `wdm apps backups list <app-id>` | List config-backup snapshots for a stack. |
| `wdm apps backups restore <app-id> <snapshot>` | Restore config files from a snapshot. |

**`wdm apps install <app-id>`**

- `--domain <host>` — public domain for the app, for example `app.example.com`.
- `--stack-path <path>` — override the default `~/docker/<app>` stack path.
- `--set KEY=VALUE` — set a catalog placeholder. Repeatable. Secret placeholders are generated and cannot be set.
- `--yes` — accept safe confirmations. This never accepts the database-risk warning.

**`wdm apps logs <app-id>`**

- `--follow`, `-f` — follow the log stream.
- `--tail N` — show the last N lines per service. `0` streams all history.
- `--service NAME` — limit output to one service. Repeatable.

**`wdm apps update <app-id>`**

- `--dry-run` — show what the update would change without applying it.
- `--yes` — accept safe confirmations.
- `--accept-database-risk` — required for apps flagged with database risk.
- `--target-version <v>` — update to a specific catalog version.

**`wdm apps restart <app-id>`**

- `--yes` — accept safe confirmations.
- `--stack-path <path>` — override the default stack path.

**`wdm apps stop-all`**

- Runs `docker compose stop` against every managed stack that has at least one running container. Containers stop but stay defined; networks, named volumes, and all data are preserved (this is not `down`).
- Targets only running apps: stacks that are already stopped are skipped and reported as "already stopped". When no app is running, it prints "No running apps to stop." and exits `0` without prompting.
- Continues on error: every targeted (running) stack is attempted even if some fail, and the command exits nonzero when any targeted stack failed.
- `--yes` — accept the safe stop confirmation without prompting.

**`wdm apps remove <app-id>`**

- `--yes` — accept safe confirmations.
- `--stack-path <path>` — override the default stack path.

**`wdm apps delete <app-id>`**

- `--confirm-name <app-id>` — required. Type the exact app id to confirm deletion.
- `--stack-path <path>` — override the default stack path.
- After `docker compose down`, and before the stack files are deleted, `delete` removes the app's `wdm`-created Docker networks best-effort. Named volumes and all data are still **kept** (never `down -v`). A network already gone counts as removed; one that cannot be removed (for example still holding an endpoint) is reported with the exact `docker network rm <name>` command and never aborts the deletion. Reinstall recreates the networks. Unlike `delete`, `remove` leaves the networks in place.

**`wdm apps backups restore <app-id> <snapshot>`**

- `--yes` — accept safe confirmations.
- `--stack-path <path>` — override the default stack path.

### Catalog

Inspect and update the local catalog of installable apps.

| Command | Description |
|---|---|
| `wdm catalog check` | Check whether a newer verified catalog is available. |
| `wdm catalog update` | Download, verify, and install a newer catalog. |
| `wdm catalog list` | List the installable apps in the catalog. |
| `wdm catalog show <app-id>` | Show full details for one catalog app. |

- `wdm catalog check` — `--channel <name>`.
- `wdm catalog update` — `--channel <name>`, `--target-version <v>`, `--yes`.
- `wdm catalog list` — `--channel <name>`.
- `wdm catalog show <app-id>` — `--channel <name>`.

### Self-update

Update the `wdm` binary itself.

| Command | Description |
|---|---|
| `wdm self-update check` | Check whether a newer verified `wdm` binary is available. |
| `wdm self-update apply` | Download, verify, and install a newer `wdm` binary. |

- `wdm self-update apply` — `--yes`, `--target-version <v>`.

`wdm` keeps the previous binary at `~/.local/bin/wdm.previous` so an update can roll back.

### Settings

View and change user settings stored in `~/.config/wdm/config.toml`.

| Command | Description |
|---|---|
| `wdm settings` | View `wdm` user settings. |
| `wdm settings set <key> <value>` | Set a `wdm` user setting. |

### Lock

Inspect and clear the global runtime lock that guards state-changing operations.

| Command | Description |
|---|---|
| `wdm lock status` | Show the global runtime lock state. |
| `wdm lock clear` | Clear a stale global runtime lock. |

- `wdm lock clear` — `--yes`.

### Uninstall

Remove `wdm` itself and tear down every managed app. This is a top-level command, not under `wdm apps`.

| Command | Description |
|---|---|
| `wdm uninstall` | Tear down every managed app and remove `wdm`'s own footprint. |

- Runs `docker compose down --rmi all` against every managed stack, removing containers and the stack's images. After every app is down it also removes the `wdm`-created Docker networks (the ones `wdm` pre-creates at install and the compose declares `external`, so `down` never removes them), then sweeps every remaining network carrying the `wdm.managed=true` label — including ones orphaned by an app you deleted earlier (whose compose file is gone) — leaving Docker clean. It then removes `wdm`'s own footprint: the config, data, and state directories and the `wdm` binary (and its `.previous` sibling).
- Named volumes and every `~/docker/<app>/` stack directory are **kept**. This is never `docker compose down -v`; no user data is deleted. Scope is wdm-managed apps and wdm's footprint only — never a system-wide Docker prune.
- Network cleanup is best-effort: a network already gone counts as removed, and a network that cannot be removed is left in place and reported with the exact `docker network rm <name>` command to run manually. It never aborts the uninstall. Networks created before label support (pre-`wdm.managed=true`) are not matched by the sweep and must be removed manually.
- Fail-closed: if any stack fails to tear down it aborts before removing anything, leaves `wdm` installed, lists the stacks that failed, and exits nonzero. On full success the `wdm` binary is already gone and the command exits `0`.
- `--yes` — accept the destructive uninstall confirmation without prompting. Without a terminal to prompt on and without `--yes`, the uninstall is declined.

## Examples

Install a stack with a public domain and a catalog placeholder:

```sh
wdm apps install nextcloud --domain cloud.example.com --set timezone=Europe/Berlin --yes
```

List every managed stack with its live runtime state. Plain output is one
tab-separated `app_id<TAB>stack_path<TAB>state` line per stack; the `--json`
envelope carries the same entries under the `apps` key, each with a live
`state` (`running`, `stopped`, `needs_attention`, or `removed`) and `needs_attention`
flag derived from real container state:

```sh
wdm apps list
wdm apps list --json
```

Get a stack's status as JSON for a script:

```sh
wdm apps status nextcloud --json
```

Preview an update without applying it:

```sh
wdm apps update nextcloud --dry-run
```

Stream the last 100 log lines from one service and follow:

```sh
wdm apps logs nextcloud --service app --tail 100 --follow
```

Remove a stack but keep its files and volumes, then permanently delete it later:

```sh
wdm apps remove nextcloud --yes
wdm apps delete nextcloud --confirm-name nextcloud
```

`remove` stops a stack and leaves its files, volumes, and networks in place, so you can reinstall or restart it. `delete` permanently removes the stack's files and directory, removes the app's `wdm`-created Docker networks best-effort (data and named volumes are still kept), and requires `--confirm-name <app-id>`.

Uninstall `wdm` and tear down every managed app, keeping all volumes and stack data:

```sh
wdm uninstall --yes
```

## Exit codes

`wdm` returns a specific exit code for each failure category.

| Code | Meaning |
|---|---|
| 0 | Success. |
| 1 | Generic failure that fits no specific category below. |
| 2 | Usage or validation error: malformed input or invalid configuration. Also the default for CLI flag and subcommand parse errors. |
| 3 | Verification failed: a release, catalog, or signature/checksum/attestation check failed. Verification fails closed. |
| 4 | Runtime lock held: another `wdm` process holds the global runtime lock. |
| 5 | Docker unavailable: Docker or the Compose plugin is unavailable. |
| 6 | Permission denied: `wdm` was invoked as root or under sudo, which it refuses by design. |
| 7 | User canceled: a confirmation prompt was dismissed. |
| 8 | Network failure: a registry, GitHub, or catalog network operation failed. |
| 9 | Migration failure: a schema or state migration step failed. |

## Runtime layout

`wdm` follows the XDG base-directory layout.

| Path | Purpose |
|---|---|
| `~/.local/bin/wdm` and `~/.local/bin/wdm.previous` | The binary and the previous version kept for self-update rollback. |
| `~/.config/wdm/config.toml` | User settings, schema-versioned. |
| `~/.local/state/wdm/runtime.lock` | The global runtime lock for state-changing operations. |
| `~/.local/state/wdm/logs/` | Logs, retained for about 30 days or 50 files. |
| `~/.local/share/wdm/catalogs/<channel>/` | Verified catalog data, per channel. |
| `~/docker/<app>/` | Each managed stack: its Compose file, `.env`, `.wdm.lock`, and `.wdm-backups/`. |

## Verification

Catalog updates, stack updates, and self-update all verify signed artifacts and fail closed. A missing or invalid signature, checksum, or attestation stops the operation. See [SECURITY.md](SECURITY.md) for the full verification procedure and the human verification commands.

See also the [README.md](README.md) for installation and the safety model.
