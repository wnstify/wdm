// Package docker owns the low-level Docker/Compose command seam used by
// internal/core. The surface is deliberately narrow: a mockable [Client]
// interface plus constructor-time dependency-injection hooks, with raw
// argv kept private to package internals.
//
//	-: [Client], closed typed [Invocation] inputs, [New],
//	  [Option], [WithRedactor], and [WithCommandExecutor] hooks.
//	-: argv-only command execution via exec.CommandContext,
//	  allowlist checks, context cancellation, and stdout/stderr redaction.
//	-: [CheckVersions] and [VersionReport] enforce Docker
//	  engine 20.10+ and Compose V2.
//	-: [ValidateComposeConfig] wraps docker compose config.
//	-: [ComposeProject], [ComposePull], [ComposeUp],
//	  [ComposeDown], and [ComposeUpOptions] wrap compose deployment
//	  (down is always run without -v).
//	-: [NetworkSpec] and [EnsureNetwork] wrap network
//	  management with create-if-missing and internal-flag handling.
//	  A spec carrying an app id stamps the PRD §10 ownership labels
//	  (wdm.managed=true, wdm.app=<app>) on newly-created networks;
//	  [RemoveNetworkIfPresent] is the idempotent, not-found-tolerant
//	  removal used by destructive delete and self-uninstall.
//	-: [InspectProjectContainers], [InspectImageDigest], and
//	  [ListProjectNamedVolumes] wrap inspection for labels, ports, image
//	  digests, and named volume listing.
//	- (this package state): [ComposeLogs], [ComposeLogEntry],
//	  [ComposeLogsOptions], and the optional [LogStreamer] capability add
//	  bounded line-streaming for `docker compose logs` (with
//	  `--no-color --timestamps` and optional tail/follow/service
//	  restriction). Every streamed line passes the client's redactor
//	  before reaching the sink; [WithStreamExecutor] injects the
//	  streaming seam for tests.
//	- delete cleanup hardening: [EnsureBindMountCleanupHelperAvailable]
//	  and [RemoveBindMountContents] add the local-only, digest-pinned
//	  helper path used when container-owned bind files block host-side
//	  stack deletion.
//
// Raw argv stays private to execClient's build/execute layer, so callers
// cannot inject command tokens.
// Import boundary: internal/docker is a low-level package. It may import
// the standard library plus pkg/types and internal/security, but not
// internal/core, internal/cli, internal/tui, internal/render,
// internal/state, or internal/catalog.
// Error mapping and redaction are centralized here so higher layers
// receive typed *types.Error values without parsing Docker stderr.
package docker
