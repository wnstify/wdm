package types

// ResourceProfile is the install-time sizing policy selected for
// catalog services.
type ResourceProfile string

const (
	// ResourceProfileRecommended selects catalog recommended resource
	// limits when host capacity allows.
	ResourceProfileRecommended ResourceProfile = "recommended"

	// ResourceProfileMin selects catalog minimum resource limits.
	ResourceProfileMin ResourceProfile = "min"
)

// ResourceOverride allows per-service user overrides for resource
// values surfaced by InstallRequest.
type ResourceOverride struct {
	// Service is the Compose service name the override applies to.
	Service string `json:"service"`

	// Memory is the memory limit override (for example "512m").
	Memory string `json:"memory,omitempty"`

	// CPUs is the CPU limit override (for example "1.5").
	CPUs string `json:"cpus,omitempty"`

	// PIDs is the pid limit override.
	PIDs int `json:"pids,omitempty"`
}

// InstallRequest carries the inputs required by Engine.Install (PRD §17).
// compile; added domain, stack-path override, placeholder values,
// and the other fields the install flow consumes.
type InstallRequest struct {
	// AppID identifies the catalog template to install.
	AppID string `json:"app_id"`

	// Domain is the user-selected public domain for the app.
	Domain string `json:"domain,omitempty"`

	// StackPath overrides the default managed stack path.
	StackPath string `json:"stack_path,omitempty"`

	// PlaceholderValues provides pre-resolved render placeholders.
	PlaceholderValues map[string]string `json:"placeholder_values,omitempty"`

	// ResourceProfile selects recommended vs minimum sizing policy.
	ResourceProfile ResourceProfile `json:"resource_profile,omitempty"`

	// ResourceOverrides provides optional per-service resource overrides.
	ResourceOverrides []ResourceOverride `json:"resource_overrides,omitempty"`
}

// InstallResult summarizes a completed install. reserved the
// type; populated additional fields (started services, exposed
// ports, rendered Compose project name, …).
type InstallResult struct {
	// AppID is the app that was installed.
	AppID string `json:"app_id"`

	// StackPath is the absolute path of the new managed stack.
	StackPath string `json:"stack_path"`

	// ComposeProject is the Compose project name used for deployment.
	ComposeProject string `json:"compose_project,omitempty"`

	// StartedServices lists services started during install.
	StartedServices []string `json:"started_services,omitempty"`

	// LocalPorts reports local published ports for the stack.
	LocalPorts []PortBinding `json:"local_ports,omitempty"`

	// PostInstallGuidance carries first-run and reverse-proxy guidance.
	PostInstallGuidance *PostInstallGuidance `json:"post_install_guidance,omitempty"`

	// Status is the post-deploy runtime status snapshot.
	Status *AppStatus `json:"status,omitempty"`
}

// UpdateRequest carries the inputs required by Engine.Update (PRD §20).
// dry-run knobs the update flow consumes.
type UpdateRequest struct {
	// AppID identifies the stack to update.
	AppID string `json:"app_id"`

	// TargetTemplateVersion requests an explicit template version target.
	TargetTemplateVersion string `json:"target_template_version,omitempty"`

	// DryRun performs planning/validation without mutating stack state.
	DryRun bool `json:"dry_run,omitempty"`
}

// UpdateResult summarizes a completed update. reports which
// services changed and whether a pre-update backup was taken (PRD §21).
type UpdateResult struct {
	// AppID is the app that was updated.
	AppID string `json:"app_id"`

	// PreviousTemplateVersion is the version before update.
	PreviousTemplateVersion string `json:"previous_template_version,omitempty"`

	// NewTemplateVersion is the version after update.
	NewTemplateVersion string `json:"new_template_version,omitempty"`

	// UpdatedServices lists services changed by the update.
	UpdatedServices []string `json:"updated_services,omitempty"`

	// RiskClassifications lists applied risk categories.
	RiskClassifications []string `json:"risk_classifications,omitempty"`

	// BackupPath is the pre-update config backup path when created.
	BackupPath string `json:"backup_path,omitempty"`

	// Status is the post-update runtime status snapshot.
	Status *AppStatus `json:"status,omitempty"`
}

// RemoveRequest carries the inputs required by Engine.Remove (PRD §19).
// grew this struct with the fields that drive Confirmer prompts.
type RemoveRequest struct {
	// AppID identifies the stack to remove.
	AppID string `json:"app_id"`

	// StackPath identifies the managed stack path to remove.
	StackPath string `json:"stack_path,omitempty"`
}

// RemoveResult summarizes a completed remove. reports preserved
// data paths so the CLI can surface them on the finish screen (PRD §19).
type RemoveResult struct {
	// AppID is the app that was removed.
	AppID string `json:"app_id"`

	// StackPath is the managed stack path for the removed app.
	StackPath string `json:"stack_path,omitempty"`

	// ComposeProject is the Compose project that was stopped.
	ComposeProject string `json:"compose_project,omitempty"`

	// PreservedPaths lists data paths intentionally left on disk.
	PreservedPaths []string `json:"preserved_paths,omitempty"`

	// RemainingNamedVolumes lists named volumes that still exist.
	RemainingNamedVolumes []string `json:"remaining_named_volumes,omitempty"`

	// RemainingNetworks lists networks intentionally left in place.
	RemainingNetworks []string `json:"remaining_networks,omitempty"`

	// Status is a post-remove status snapshot when available.
	Status *AppStatus `json:"status,omitempty"`
}

// PortBinding describes one published container port.
type PortBinding struct {
	// Service is the Compose service name that exposes the port.
	Service string `json:"service,omitempty"`

	// HostIP is the host bind address for the port.
	HostIP string `json:"host_ip,omitempty"`

	// HostPort is the host port exposed by Docker.
	HostPort int `json:"host_port"`

	// ContainerPort is the container port published by Docker.
	ContainerPort int `json:"container_port"`

	// Protocol is the transport protocol ("tcp"/"udp").
	Protocol string `json:"protocol,omitempty"`
}

// PangolinGuidance carries reverse-proxy next-step guidance.
type PangolinGuidance struct {
	// TargetURL is the local service URL to proxy.
	TargetURL string `json:"target_url,omitempty"`

	// RecommendedSubdomain is the suggested proxy subdomain.
	RecommendedSubdomain string `json:"recommended_subdomain,omitempty"`

	// Notes carries optional operator guidance lines.
	Notes []string `json:"notes,omitempty"`
}

// PostInstallGuidance carries first-run guidance after successful install.
type PostInstallGuidance struct {
	// LocalTargetURL is the local URL users can open immediately.
	LocalTargetURL string `json:"local_target_url,omitempty"`

	// Pangolin carries reverse-proxy guidance for public exposure.
	Pangolin *PangolinGuidance `json:"pangolin_guidance,omitempty"`

	// FirstRunNotes are optional first-run checklist notes.
	FirstRunNotes []string `json:"first_run_notes,omitempty"`

	// GeneratedCredentials are per-install secrets shown to the operator
	// exactly once on the interactive finish screen. wdm persists only the
	// one-way hash in .env; the plaintext is never logged, never written to
	// disk, and never serialized to JSON (PRD §24).
	GeneratedCredentials []GeneratedCredential `json:"-"`
}

// GeneratedCredential is a single one-time secret surfaced to the operator
// on the finish screen. wdm persists only a one-way hash of the secret;
// the [GeneratedCredential.Value] plaintext is shown once and cannot be
// recovered. The whole field is excluded from JSON output (PRD §24) — in-
// process consumers (TUI, CLI plain finish, future GUI) read this struct
// directly.
type GeneratedCredential struct {
	Label string // "<App name> <placeholder>", e.g. "Vaultwarden ADMIN_TOKEN"
	Value string // plaintext, shown once
	Note  string // e.g. "Store this now — it cannot be recovered."
}
