// Package core implements [pkg/engine.Engine] for wdm (PRD §29, §37). It
// is the orchestrator behind the GUI-facing engine contract, gluing
// together the other internal/* siblings: internal/state for the read
// path (List, Settings) and internal/docker, internal/render,
// internal/security, internal/system plus the runtime.lock
// acquire/release dance for the write path.
// Read path and construction:
//   - [New] eagerly loads config.toml (via internal/state.LoadConfig) and
//     resolves XDG paths so the returned engine is ready for List and
//     Settings immediately.
//   - [Engine.List] scans the configured stack base (via
//     internal/state.ScanStacks) into []types.AppInfo. Corrupt.wdm.lock
//     files surface as WARN-level slog entries on the engine's logger
//     (PRD §26).
//   - [Engine.ListStatus] returns the same stack set as List, each carrying
//     a LIVE runtime summary derived from Docker container inspection
//     (PRD §18). It is the lightweight list companion to Status: it skips
//     the per-stack compose-config validation shell and never acquires the
//     runtime lock, runs the per-stack inspects concurrently with a bounded
//     pool, and sorts the result by app id for deterministic output.
//   - [Engine.Settings] returns a defensive copy of the loaded settings.
//   - [Engine.Close] is idempotent; it holds no resources directly, so it
//     just marks the engine closed and subsequent calls fail with
//     [ErrClosed].
//
// Write and streaming path:
//   - [Engine.Install] owns the full PRD §17 install path. Under the held
//     global runtime.lock it probes host resources (internal/system),
//     loads and validates the catalog app, plans the stack path, resolves
//     non-secret placeholders plus built-in values, checks localhost
//     ports once, selects catalog resource limits with recommended-to-min
//     fallback from the current host probe only,
//     generates secret placeholders (internal/security), and renders.env
//     / label-injected Compose / additional_files plus the post-install
//     guidance in memory. It
//     verifies generated secrets do not leak into non-secret artifacts or
//     guidance, validates the rendered Compose via `docker compose config
//     --quiet` against a private tempdir copy before exposure (PRD §13),
//     and writes docker-compose.yml,.env, and additional_files under the
//     per-stack flock via internal/state atomic writes. The flock stays
//     held across confirm → networks → deploy → manifest write →
//     release. It calls the [types.Confirmer] with the ports, volumes,
//     and networks the deployment will touch (PRD §17 step 11; a nil
//     confirmer refuses past the confirmation step, a decline maps to
//     types.ErrCodeUserCanceled), pre-creates catalog-declared Docker
//     networks via internal/docker.EnsureNetworkReport with internal-flag drift
//     rejected, re-checks localhost ports
//     immediately before deploy, deploys
//     via `docker compose up -d`, captures image digests opportunistically
//     persists the full.wdm.lock manifest through the
//     held flock fd as the commit point (PRD §30), verifies post-deploy
//     status by Compose project and wdm labels (PRD §18), and returns
//     [types.InstallResult] with Compose project, ports, status, and
//     Pangolin guidance. Any fault between file exposure and the manifest
//     fsync rolls back only this install's Docker resources (a safe
//     compose down, project-labeled volumes, and its own networks in
//     reverse order) and then removes the partial fresh-install files;
//     post-commit
//     verification trouble marks the result needs-attention rather than
//     failing the install. The per-operation Docker client is built
//     through an injectable factory that receives an active redactor
//     carrying the generated install secrets so Docker stderr is scrubbed
//     before any sink.
//   - [Engine.Status] fuses the full PRD §18 condition set for one
//     managed stack. It refuses unmanaged directories and uninstalled
//     apps with typed usage-validation errors BEFORE any Docker call
//     (PRD §10), reads the stack's.wdm.lock through
//     internal/state.TryReadStackLock — a non-blocking shared-flock read,
//     so a stack mid-operation surfaces a types.ErrCodeRuntimeLockHeld
//     refusal instead of stalling behind the writer's flock (PRD §26
//     read-only clause) — inspects containers by the manifest's Compose
//     project plus wdm.managed / wdm.app labels, and fuses
//     missing-container, unexpected-exit, restart-loop, unhealthy,
//     Compose-validation-failure, lock-port-mismatch,
//     failed-last-operation (nil last_successful_operation), and
//     stale-runtime-lock (held flock with a dead holder PID or a hold
//     older than 24h, probed read-only via
//     internal/state.ProbeRuntimeLock) into the [types.AppStatus]
//     running-or-needs-attention shape. The path is strictly read-only:
//     no runtime.lock acquisition, no file writes, no lock mutation.
//     Container-inspection failures propagate unchanged so
//     internal/docker's typed mapping stays authoritative, while a failed
//     compose-config validation with a live ctx is fused as a condition
//     rather than an error.
//   - [Engine.Logs] streams `docker compose logs` for one managed stack
//     through the [types.LogLineFn] callback (PRD §24). It shares Status's
//     managed-only ordering — manifest resolution and image-pin service
//     validation refuse unmanaged directories, uninstalled apps, busy
//     stacks, and unknown services BEFORE any Docker call (PRD §10, §26) —
//     then inspects the project's containers to build the managed
//     container-name → service map (wdm.managed / wdm.app labels) and
//     streams with optional tail/follow/service restriction through
//     internal/docker.ComposeLogs. The docker client redacts each line
//     before parsing; lines from containers outside the managed map and
//     Compose's own unprefixed diagnostics are dropped. The path is
//     strictly read-only (no runtime.lock, no writes); context
//     cancellation tears the stream down via SIGTERM and surfaces as a
//     typed user-canceled error.
//   - [Engine.Update] owns the PRD §20 check-planning stage (steps 1-5).
//     Under the held global runtime.lock it resolves the managed stack
//     through Status's non-blocking shared-flock read posture — unmanaged
//     directories and uninstalled apps refuse with typed usage-validation
//     errors, busy stacks with types.ErrCodeRuntimeLockHeld, corrupt
//     manifests with wrapped types.ErrStaleState, all before anything
//     else (PRD §9) — loads the selected catalog
//     channel as the only update-candidate source, validates an explicit target
//     template version against the catalog, diffs the manifest's image
//     pins against the catalog pins per service, groups the candidate
//     into the PRD §20 risk classes from the catalog's schema-validated
//     risk_classification array, and
//     surfaces per-service old → new image references plus the check
//     summary on the types.StepUpdatePlanning progress stream. DryRun
//     returns the populated check result (previous/candidate versions,
//     changed services, risk grouping) without touching files, backups,
//     Docker, or the Confirmer.
//     When an update is available and the catalog risk classification
//     includes "database", apply gates on the exact PRD §20 database-risk
//     text through the [types.Confirmer] BEFORE backup (a decline maps to
//     types.ErrCodeUserCanceled with no on-disk side effect; no-op and
//     non-database applies are unaffected). After that gate clears, apply
//     acquires the per-stack exclusive flock
//     (internal/state.AcquireStackLock) and holds it across backup →
//     rewrite → validate → confirm → networks → pull → recreate →
//     manifest write → status → prune → release. It reconfirms the
//     managed identity through the held fd, snapshots the config files
//     into .wdm-backups via internal/state.CreateConfigBackup BEFORE any
//     byte changes, and atomically re-renders
//     compose, .env (secret-mode 0o600), and additional files. The render
//     reuses non-secret values and regenerable: false secrets
//     byte-identically from the existing .env through
//     internal/state.ReadStackEnv; a missing reuse value refuses. It regenerates regenerable: true
//     secrets, and re-resolves UID / GID fresh; the COMBINED generated
//     and reused secret literals drive the operation Docker client's
//     redactor (so Compose stderr is scrubbed of reused install-time
//     secrets) and the non-secret leak verification. The rewritten bytes
//     are validated via internal/docker.ValidateComposeConfig against a
//     private tempdir copy (PRD §20 step 9), then the [types.Confirmer]
//     authorizes the recreate after tag rewrite and before pull with the
//     image changes, ports, volumes, and backup
//     path; a database-risk update confirms twice (warning then
//     recreate), a non-database update once. Catalog networks are
//     pre-created, then internal/docker.ComposePull and
//     internal/docker.ComposeUp with ForceRecreate deploy the new
//     template. Image digests are captured
//     opportunistically and the .wdm.lock manifest is
//     rewritten through the held flock fd as the commit point (PRD §30):
//     new template/catalog version, image pins,
//     last_successful_operation kind=update, and the snapshot appended to
//     backup_history, with the installed domain, local ports, recommended
//     resources, and Compose project preserved (update does not re-plan
//     ports or resources; protocol step 5 lists the second port-check
//     pass for Install only). Post-commit status verification fuses the
//     install-time PRD §18 subset and marks the result needs-attention on
//     trouble rather than failing the durable update; retention pruning
//     then runs with this run's snapshot pinned, a
//     prune failure logged rather than fatal. A no-op (up-to-date) apply
//     still backs up, re-renders, and redeploys so the rewrite becomes
//     live.
//     Any fault after the rewrite exposed the new bytes and before the
//     commit point — a validation failure, a confirmer decline, a network
//     failure, or a pull / recreate failure — restores the step-3
//     snapshot byte-for-byte via internal/state.RestoreConfigBackup
//     and surfaces a typed error: the induced-failure case is
//     types.ErrCodeGeneric with a hint naming the restored backup path
//     a decline keeps types.ErrCodeUserCanceled
//     and a restore that itself fails fails closed by joining
//     both causes. The restore runs on the contextless
//     restore primitive so a canceled operation ctx — itself a trigger —
//     cannot interrupt it; no user-facing string ever says
//     "rollback". A restored-but-broken stack then
//     surfaces as needs-attention through the existing §18
//     runtime-vs-config conditions on a subsequent Status call.
//     Post-commit faults never restore.
//   - [Engine.Remove] is the PRD §19 safe-removal path. Under the held
//     global runtime.lock, planning resolves the managed stack through
//     Status's non-blocking shared-flock read posture — unmanaged
//     directories and uninstalled apps refuse with typed usage-validation
//     errors, busy stacks with types.ErrCodeRuntimeLockHeld, corrupt
//     manifests with wrapped types.ErrStaleState, a manifest missing its
//     compose project with a typed usage-validation refusal, all before
//     any Docker call (PRD §9, §10). Resolution stays
//     AppID-driven like Status and Update, so a provided stack path is
//     verified against the resolved managed stack and a mismatch refuses
//     fail-closed. Planning lists the stack's
//     Compose-project named volumes via
//     internal/docker.ListProjectNamedVolumes and resolves
//     the catalog-declared networks safe removal leaves in place
//     both reads are opportunistic — a
//     transient inspect failure or an unavailable catalog WARN-logs and
//     reports an empty list rather than blocking removal of a stack wdm
//     already manages, while context cancellation and an
//     unreachable daemon still propagate as typed errors. Execution then
//     takes the per-stack exclusive flock (protocol step 2), reconfirms
//     managed identity through the held fd, and asks the Confirmer to
//     authorize the removal immediately before `docker compose down` with
//     a payload naming the app, stack path, and the files, volumes, and
//     networks kept. A nil confirmer refuses with usage-validation, a decline
//     maps to types.ErrCodeUserCanceled and leaves zero trace, and a confirmer
//     error propagates wrapped. It
//     runs `docker compose down`, records
//     last_successful_operation kind="remove" through the held fd as the
//     PRD §30 commit point with every other manifest field preserved and
//     the .wdm.lock kept on disk, verifies the post-down
//     status (no managed container remaining is success with State
//     "removed"; a broken inspection or a lingering container marks the
//     result needs-attention with the status_check_failed reason rather
//     than failing the durable removal), and re-lists the surviving named
//     volumes so types.RemoveResult proves what safe removal preserved. A
//     down failure surfaces the docker-layer typed error and leaves the
//     manifest unmarked and the files byte-identical (no restore is owed —
//     remove rewrites no config bytes). The path takes no pre-remove
//     config backup (pkg/types defines no StepRemoveBackup and remove
//     exposes no new bytes to undo), never reads.env content, never
//     renders, and surfaces no "rollback" wording.
//     Progress rides the frozen step_remove_* stream.
//   - [Engine.UpdateSettings] validates and persists config.toml under
//     the held global runtime.lock; see its own doc for the validation
//     matrix and write posture.
//
// Construction flow ([New]):
//  1. Apply functional options (the [With*] setters in options.go).
//  2. When [WithLogger] was not supplied, build the production default
//     logger via buildDefaultLogger: a [slog.NewJSONHandler] wrapped in
//     [internal/logging.NewRedactingHandler] over
//     [internal/security.NewActiveRedactor] so every record passes through
//     the active redactor before reaching the sink (PRD §11, §24). A
//     caller-supplied logger is used as-is.
//  3. Resolve unset paths to XDG defaults per
//     "On-disk layout":
//     - configPath → $XDG_CONFIG_HOME/wdm/config.toml
//     - stateDir → $XDG_STATE_HOME/wdm
//     - dataDir → $XDG_DATA_HOME/wdm
//     Relative XDG_* values are ignored per the XDG Base Directory
//     spec (security posture against PATH-style injection); a
//     missing $HOME aborts construction.
//  4. Load config.toml via state.LoadConfig. A missing file falls back to
//     PRD §34 defaults so a first-launch user without a config can still
//     run "wdm apps list"; an invalid config surfaces as a wrapped
//     types.ErrConfigInvalid so cmd/wdm maps it to exit code 2 (PRD §27
//  5. Resolve the stack base from settings.BaseStackPath (leading ~/
//     expanded against $HOME) unless WithStackBaseDir overrode it.
//  6. Construct the *Engine and return it. cmd/wdm uses the concrete
//     type, but the engine.Engine interface is satisfied via the
//     compile-time assertion in pkg/engine/new.go.
//
// Concurrency: every [Engine] method is safe for concurrent use by
// multiple goroutines. The only mutable field is the [atomic.Bool]
// closed flag set by Close and checked on every method's entry.
// Import boundary: per, internal/core is the
// orchestrator and MAY import any other internal/* sibling. It MUST be
// imported only by cmd/wdm and pkg/engine's public bridge; internal/tui
// and internal/cli go through pkg/engine (depguard rules tui-uses-engine
// and cli-uses-engine enforce this). Other pkg/* packages MUST NOT depend
// on this package.
package core
