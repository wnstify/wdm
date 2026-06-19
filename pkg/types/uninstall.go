package types

// ConfirmationKindUninstallDestructive is the [Confirmation.Kind] the
// self-uninstall flow carries (PRD §39). Self-uninstall tears down every
// managed stack and then removes wdm's own footprint including the running
// binary, so it is gated like the destructive deletion of §19 — never like
// the safe remove. The modal renders the managed stacks that will be torn
// down, the data paths that will be KEPT (named volumes and ~/docker/<app>/
// directories), and the wdm footprint that will be removed from this
// payload. It is an exported const so the CLI and TUI leaves reference it
// without re-typing the literal, mirroring
// [ConfirmationKindDeleteDestructive].
const ConfirmationKindUninstallDestructive = "uninstall_destructive"

// UninstallRequest carries the inputs for Engine.Uninstall, the top-level
// self-uninstall operation (PRD §39, issue #29). The request is
// intentionally argless, mirroring [StopAllRequest]: Uninstall always
// targets the full managed set discovered under the configured stack base
// plus wdm's own on-disk footprint. There is no per-app or partial selector.
type UninstallRequest struct{}

// TornDownApp is one managed stack's teardown outcome inside an
// [UninstallResult]. It names the managed stack and, when teardown failed,
// carries the redacted failure detail. It mirrors [StoppedApp]: a successful
// teardown leaves Error empty.
type TornDownApp struct {
	// AppID is the managed app the outcome describes.
	AppID string `json:"app_id"`

	// ComposeProject is the Compose project whose containers and images were
	// targeted by `docker compose down --rmi all`. It may be empty when the
	// stack manifest was unreadable before the teardown could run.
	ComposeProject string `json:"compose_project,omitempty"`

	// Error holds the failure detail when this stack failed to tear down. It
	// is empty on success. The message carries no secret values (stack path,
	// Compose project, and the docker-layer reason only).
	Error string `json:"error,omitempty"`
}

// RetainedNetwork is one wdm-managed network the best-effort network cleanup
// could not remove after teardown (PRD §39). Name is the network's real
// (substituted) name and Reason is the redacted docker-layer detail. A
// retained network never aborts the uninstall: footprint removal still
// proceeds, and the frontends surface the exact `docker network rm <name>`
// command so the operator can finish the cleanup manually.
type RetainedNetwork struct {
	// Name is the wdm-managed network that was left behind.
	Name string `json:"name"`

	// Reason holds the redacted docker-layer detail explaining why the
	// network could not be removed. It carries no secret values.
	Reason string `json:"reason"`
}

// UninstallResult summarizes a self-uninstall run (PRD §39). Self-uninstall
// is fail-closed: every managed stack is torn down with `docker compose down
// --rmi all` (NEVER -v), and the wdm footprint is removed ONLY when EVERY
// stack tears down cleanly. When any stack fails, Failed is populated, the
// footprint is left untouched (RemovedPaths empty), and wdm stays installed.
// On a clean run TornDown lists the torn-down stacks and RemovedPaths lists
// the footprint that was removed; KeptDataPaths always reports the data that
// was preserved (named volumes and ~/docker/<app>/ stack directories), since
// self-uninstall never deletes user data.
//
// After every stack tears down cleanly and before any footprint removal,
// self-uninstall removes the wdm-created Docker networks best-effort:
// RemovedNetworks lists the networks dropped, and RetainedNetworks lists the
// ones that could not be removed (each with a reason). Network cleanup is
// best-effort and NEVER triggers the fail-closed abort — footprint removal
// proceeds regardless.
type UninstallResult struct {
	// TornDown lists the managed stacks whose containers and images were
	// removed cleanly.
	TornDown []TornDownApp `json:"torn_down,omitempty"`

	// Failed lists the managed stacks whose teardown failed, each carrying
	// its redacted failure detail in [TornDownApp.Error]. A non-empty Failed
	// means the operation aborted before removing any wdm footprint.
	Failed []TornDownApp `json:"failed,omitempty"`

	// KeptDataPaths lists the on-disk data paths self-uninstall preserved —
	// the per-app ~/docker/<app>/ stack directories. Named volumes are also
	// preserved (the -v prohibition stays absolute) but are Docker objects,
	// not filesystem paths, so they are reported through the confirmation
	// payload rather than here.
	KeptDataPaths []string `json:"kept_data_paths,omitempty"`

	// RemovedPaths lists the wdm footprint paths that were removed — the
	// config dir, the data/share dir, the state dir (with the runtime lock),
	// the running binary, and its .previous sibling. It is empty when the
	// operation aborted on a teardown failure.
	RemovedPaths []string `json:"removed_paths,omitempty"`

	// RemovedNetworks lists the wdm-created Docker networks removed
	// best-effort after every stack tore down cleanly. wdm pre-creates these
	// networks at install and they are declared external in the rendered
	// compose, so `docker compose down` never owns or removes them. Names are
	// deduplicated across stacks; a network already absent counts as removed.
	RemovedNetworks []string `json:"removed_networks,omitempty"`

	// RetainedNetworks lists the wdm-created networks that could not be
	// removed (each with a redacted reason). A retained network never aborts
	// the uninstall; the frontends surface the manual `docker network rm`
	// command so the operator can finish the cleanup.
	RetainedNetworks []RetainedNetwork `json:"retained_networks,omitempty"`
}
