package types

import "time"

// AppInfo is a single entry returned by Engine.List. It carries the
// long-lived facts about a managed stack, read from .wdm.lock (PRD §9).
// Runtime status — running, needs_attention, … — lives in AppStatus,
// populated by Engine.Status (PRD §18), not here.
type AppInfo struct {
	// AppID is the stable catalog identifier (e.g. "vaultwarden").
	AppID string `json:"app_id"`

	// TemplateName is the human-readable template name from the catalog.
	TemplateName string `json:"template_name"`

	// StackPath is the absolute directory path of the managed stack.
	StackPath string `json:"stack_path"`

	// CatalogChannel is the channel the stack was installed from
	// (currently always "stable"; PRD §22).
	CatalogChannel string `json:"catalog_channel"`

	// CatalogVersion is the catalog manifest version the stack was
	// installed against.
	CatalogVersion string `json:"catalog_version"`

	// LastSuccessfulOperation is the most recent lifecycle operation
	// that completed cleanly. A nil pointer indicates an interrupted
	// install (PRD §9).
	LastSuccessfulOperation *Operation `json:"last_successful_operation"`

	// NeedsAttention is true when the stack matches any condition in
	// PRD §18 (missing container, restart loop, unhealthy, …). The cheap
	// Engine.List path leaves it false because it never touches Docker;
	// Engine.ListStatus and Engine.Status are the live signals that
	// populate it from container inspection.
	NeedsAttention bool `json:"needs_attention"`
}

// AppRuntimeStatus is one entry returned by Engine.ListStatus: the
// long-lived AppInfo facts read from .wdm.lock plus a live runtime
// summary derived from Docker container inspection (PRD §18). Unlike
// AppInfo (whose NeedsAttention stays zero because the cheap List path
// never touches Docker), the State, NeedsAttention, and AttentionReasons
// here reflect the current container state at the moment ListStatus ran.
// The summary is lightweight by design: it derives State from container
// inspection and the manifest alone, skipping the per-stack compose-config
// validation shell the full Engine.Status runs, and it never acquires the
// runtime lock (PRD §26 read-only posture).
type AppRuntimeStatus struct {
	// AppInfo carries the long-lived stack facts read from .wdm.lock.
	AppInfo

	// State is the live coarse status label derived from container
	// inspection: "running", "stopped", "needs_attention", or "removed". It
	// uses the same vocabulary Engine.Status emits. "stopped" marks an app
	// whose expected managed containers all exist but none are running — a
	// cleanly stopped stack, distinct from the "needs_attention" trouble
	// state.
	State string `json:"state"`

	// AttentionReasons lists machine-readable PRD §18 reason IDs when the
	// stack needs attention; empty when State is "running".
	AttentionReasons []string `json:"attention_reasons,omitempty"`
}

// Operation records a completed lifecycle event for a managed stack.
// Stored as last_successful_operation in.wdm.lock (PRD §9).
type Operation struct {
	// Kind is one of "install", "update", "remove".
	Kind string `json:"kind"`

	// At is the UTC timestamp at which the operation completed.
	At time.Time `json:"at"`

	// WDMVersion is the wdm binary version that performed the operation.
	WDMVersion string `json:"wdm_version"`
}

// AppStatus is the runtime status of a managed stack, returned by
// Engine.Status (PRD §18). reserved the type; wired
// Docker introspection to make Status live.
type AppStatus struct {
	// AppID identifies the stack the status belongs to.
	AppID string `json:"app_id"`

	// State is a coarse status label. PRD §18 surfaces "running",
	// "stopped", and "needs attention" to users; it is a free-form string,
	// not a closed enum (Engine.Status sets the values it emits). "stopped"
	// marks an app whose expected managed containers all exist but none are
	// running.
	State string `json:"state"`

	// Message is an optional one-line explanation shown alongside State.
	Message string `json:"message,omitempty"`

	// ComposeProject is the Compose project associated with the stack.
	ComposeProject string `json:"compose_project,omitempty"`

	// StackPath is the managed stack directory path.
	StackPath string `json:"stack_path,omitempty"`

	// NeedsAttention summarizes whether any PRD §18 attention condition
	// is currently true for this stack.
	NeedsAttention bool `json:"needs_attention"`

	// AttentionReasons lists machine-readable reason IDs for
	// needs-attention status.
	AttentionReasons []string `json:"attention_reasons,omitempty"`

	// Services carries per-service runtime status details.
	Services []ServiceStatus `json:"services,omitempty"`

	// LocalPorts carries aggregated host-local published ports.
	LocalPorts []PortBinding `json:"local_ports,omitempty"`

	// UpdatedAt is when this status snapshot was collected.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// ServiceStatus is per-service runtime state inside AppStatus.
type ServiceStatus struct {
	// Service is the Compose service name.
	Service string `json:"service"`

	// ContainerName is the concrete container name when known.
	ContainerName string `json:"container_name,omitempty"`

	// State is the service runtime state (running/exited/…).
	State string `json:"state"`

	// Health is the health status when Docker reports one.
	Health string `json:"health,omitempty"`

	// NeedsAttention marks service-level attention conditions.
	NeedsAttention bool `json:"needs_attention,omitempty"`

	// Message is an optional one-line service status detail.
	Message string `json:"message,omitempty"`

	// PublishedPorts are ports published by this service.
	PublishedPorts []PortBinding `json:"published_ports,omitempty"`
}
