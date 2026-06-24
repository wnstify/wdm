// Package engine is the public, GUI-facing Go API for wdm (PRD §29,
// §37). It declares the Engine interface, the ProgressFn and LogLineFn
// callback types (aliases of [types.ProgressFn] / [types.LogLineFn]),
// the Confirmer authorization hook for destructive actions (alias of
// [types.Confirmer]), and the functional Option pattern consumed by New.
// Import boundary: this package depends only on the standard library,
// pkg/types, and internal/core. It must
// not import Bubble Tea, Cobra, or any internal/* sibling other than
// core, so future GUI builds linking only pkg/engine + pkg/types stay
// supported (PRD §37). The depguard
// "pkg-engine-stays-uiless" rule enforces the boundary.
// the matching core.Option values and delegates construction to
// core.New. Public callers (cmd/wdm, future GUI) never reach into
// internal/core directly. The returned Engine satisfies the interface
// via the compile-time type assertion in new.go.
// Public surface:
//   - [Engine] — read (List, ListStatus, Status, Logs, ResourceSettings,
//     ViewEnvRedacted, ValidateStack, LogPath), write (Install, Update,
//     Remove, Restart, RedeployStack, Reconfigure, EnsureUserOverride,
//     EnsureUserEnv, RewireStack, StopAll, Uninstall), settings (Settings,
//     UpdateSettings), lifecycle
//     (Close)
//   - [ResolveEditorArgv] — pure helper that resolves the user's editor
//     ($VISUAL → $EDITOR → nano) into a typed, no-shell argv; usable by
//     both the CLI and TUI without importing internal/system
//   - [Confirmer] — authorizes destructive actions (PRD §37); alias of
//     [types.Confirmer]
//   - [ProgressFn] — long-running operation progress callback (PRD §37);
//     alias of [types.ProgressFn]
//   - [LogLineFn] — per-line callback used by Engine.Logs; alias of
//     [types.LogLineFn]
//   - [Option] and the With* constructors — functional options for [New]
//   - [New] — engine constructor; validates config eagerly and bridges
//     to internal/core
package engine
