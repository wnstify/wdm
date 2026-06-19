// Package types defines the data contracts exchanged between wdm's
// public engine API (pkg/engine), its internal implementation
// (internal/core), and any UI layer — CLI, TUI, or a future GUI talking
// over IPC. See PRD §29 (package layout) and §37 (GUI compatibility).
// Every exported type in this package is JSON-serializable and
// UI-agnostic. The package depends only on the Go standard library; the
// depguard rule in .golangci.yml forbids importing anything from
// internal/ or any third-party UI/runtime library.
// Type roll-call:
//   - Envelope, NewEnvelope — JSON envelope used by --json and IPC (PRD §32)
//   - Error, ErrorCode, IsCode — typed error payload mapped to CLI exit codes (PRD §27, §37)
//   - ErrNotImplemented,
//     ErrConfigInvalid,
//     ErrStaleState — sentinel errors used across the engine
//   - Settings — user-configurable settings persisted at
//     ~/.config/wdm/config.toml (PRD §34)
//   - AppInfo, AppStatus,
//     ServiceStatus, Operation — managed-stack data returned by
//     Engine.List / Engine.Status (PRD §9, §18)
//   - InstallRequest/Result,
//     UpdateRequest/Result,
//     RemoveRequest/Result — lifecycle method payloads (PRD §17, §19, §20)
//   - RestartRequest/Result,
//     StopAllRequest/Result, StoppedApp — batch "stop all apps"
//     payloads (issue #27)
//   - UninstallRequest/Result, TornDownApp — self-uninstall payloads
//     (PRD §39, issue #29)
//     ValidationResult,
//     BackupInfo,
//     RestoreBackupRequest/Result,
//     CatalogQuery, CatalogAppQuery,
//     CatalogApp + projections,
//     DeleteRequest/Result,
//     RuntimeLockStatus — engine-gap payloads (PRD §18, §19,
//   - ConfirmationKindDeleteDestructive — the DeleteApp Confirmation.Kind (PRD §19)
//   - ConfirmationKindUninstallDestructive — the Uninstall Confirmation.Kind (PRD §39)
//   - CatalogUpdateQuery/Status,
//     CatalogUpdateRequest/Result,
//     CatalogChange,
//     SelfUpdateQuery/Status,
//     SelfUpdateRequest/Result,
//     ImageUpdateQuery,
//     ImageUpdateReport,
//     ImageUpdateCandidate — trust/distribution payloads:
//     catalog update, binary self-update, and the Go-native registry image
//     check (PRD §14, §22, §23)
//   - ConfirmationKindCatalogUpdate,
//     ConfirmationKindSelfUpdate — the ApplyCatalogUpdate / ApplySelfUpdate
//     Confirmation.Kind values (PRD §14, §22)
//   - ResourceProfile,
//     ResourceOverride,
//     PortBinding,
//     PostInstallGuidance,
//     PangolinGuidance — lifecycle support types used by install,
//     update, remove, and status payloads
//   - LogsRequest, LogLine — Engine.Logs streaming payloads (PRD §24)
//   - Progress,
//     StepInstall* / StepUpdate* /
//     StepRemove* / StepRestart* /
//     StepStopAll* / StepUninstall* /
//     StepRestore* / StepDelete* /
//     StepCatalogUpdate* /
//     StepSelfUpdate*
//     constants — on-the-wire equivalent of the ProgressFn
//     callback args, used by the future GUI/IPC (PRD §37)
//   - Confirmation — payload passed to a Confirmer (PRD §37)
//
// fields additively; added the engine-gap request/result shapes;
// image-check shapes.
// Any change to a JSON tag, field name, stable progress step constant
// value, or the EnvelopeSchema constant is a wire-breaking change that
// requires a schema version bump.
package types
