package types

// RestartRequest carries the inputs required by Engine.Restart (PRD
// §18:416 "Restart app"). ships plain restart semantics
// the engine runs docker compose restart, which stops
// and starts the same containers without re-reading the Compose file.
// A restart never re-renders templates and never touches config files
// or backups.
type RestartRequest struct {
	// AppID identifies the managed stack to restart.
	AppID string `json:"app_id"`

	// StackPath is an optional fail-closed cross-check: when set it must
	// match the AppID-resolved managed stack or the engine refuses
	// before any Docker call, mirroring RemoveRequest's guard. It is a
	// guard, never an alternate resolution path.
	StackPath string `json:"stack_path,omitempty"`

	// There is deliberately no Services field: v1 restarts the whole
	// stack only.
}

// RestartResult summarizes a completed restart. The post-restart
// runtime status is fused from the PRD §18 condition set so the caller
// can render the same needs-attention view Status produces.
type RestartResult struct {
	// AppID is the app that was restarted.
	AppID string `json:"app_id"`

	// ComposeProject is the Compose project whose containers were
	// restarted.
	ComposeProject string `json:"compose_project,omitempty"`

	// RestartedServices lists the services the restart touched.
	RestartedServices []string `json:"restarted_services,omitempty"`

	// Status is the post-restart runtime status snapshot.
	Status *AppStatus `json:"status,omitempty"`
}
