package types

// ReconfigureRequest carries the inputs required by Engine.Reconfigure
// (issue #28): a post-install change to one managed service's resource
// limits (memory, CPUs, PIDs). The engine rewrites only the resource
// vars in the stack's .env, preserves every secret and unrelated value
// byte-for-byte, re-renders, validates, and recreates the container.
//
// Each limit is a pointer to model "leave unchanged" (nil) distinctly
// from "set to this value" (non-nil) — unlike the install-time
// [ResourceOverride], whose empty-string/zero sentinels cannot express
// the difference. A request with all three limits nil changes nothing
// and is refused as a usage error: callers use the read-only
// current-values view instead.
type ReconfigureRequest struct {
	// AppID identifies the managed stack to reconfigure.
	AppID string `json:"app_id"`

	// Service is the Compose service whose resource limits change. It
	// must match a service the app's catalog entry declares a resource
	// band for, and that band must allow overrides.
	Service string `json:"service"`

	// StackPath is an optional fail-closed cross-check: when set it must
	// match the AppID-resolved managed stack or the engine refuses before
	// any change, mirroring [RestartRequest.StackPath]. It is a guard,
	// never an alternate resolution path.
	StackPath string `json:"stack_path,omitempty"`

	// Memory is the new memory limit (Docker "<integer><b|k|m|g>" form,
	// for example "1g"). nil leaves the installed value unchanged.
	Memory *string `json:"memory,omitempty"`

	// CPUs is the new CPU quota (decimal string, for example "1.5"). nil
	// leaves the installed value unchanged.
	CPUs *string `json:"cpus,omitempty"`

	// PIDs is the new pid limit. nil leaves the installed value
	// unchanged.
	PIDs *int `json:"pids,omitempty"`
}

// ResourceSettings is the read-only view behind the no-flags
// `wdm resources <app>` invocation: per overridable service, the
// resource limits currently in effect (read from the stack's .env) and
// the catalog's allowed bands (min / recommended / max for memory and
// CPUs, default / max for PIDs). It feeds the CLI's current-values
// display so a user can see what they may change before changing it.
type ResourceSettings struct {
	// AppID is the app the settings describe.
	AppID string `json:"app_id"`

	// Services lists one entry per service the catalog declares a
	// resource band for, including services whose bands forbid overrides
	// (Adjustable reports which may change).
	Services []ResourceServiceSettings `json:"services"`
}

// ResourceServiceSettings is the current limits and allowed bands for a
// single service in a [ResourceSettings] view.
type ResourceServiceSettings struct {
	// Service is the Compose service name.
	Service string `json:"service"`

	// Adjustable reports whether the catalog allows overriding this
	// service's resource limits (the allow_override band flag).
	Adjustable bool `json:"adjustable"`

	// CurrentMemory is the memory limit currently in effect.
	CurrentMemory string `json:"current_memory,omitempty"`

	// CurrentCPUs is the CPU quota currently in effect.
	CurrentCPUs string `json:"current_cpus,omitempty"`

	// CurrentPIDs is the pid limit currently in effect.
	CurrentPIDs int `json:"current_pids,omitempty"`

	// MemoryMin, MemoryRecommended, MemoryMax bound the memory band.
	MemoryMin         string `json:"memory_min,omitempty"`
	MemoryRecommended string `json:"memory_recommended,omitempty"`
	MemoryMax         string `json:"memory_max,omitempty"`

	// CPUsMin, CPUsRecommended, CPUsMax bound the CPU band.
	CPUsMin         string `json:"cpus_min,omitempty"`
	CPUsRecommended string `json:"cpus_recommended,omitempty"`
	CPUsMax         string `json:"cpus_max,omitempty"`

	// PIDsDefault, PIDsMax bound the pids band (pids has no min — it is a
	// containment cap, not a sizing requirement).
	PIDsDefault int `json:"pids_default,omitempty"`
	PIDsMax     int `json:"pids_max,omitempty"`
}

// ReconfigureResult summarizes a completed reconfigure. It reports the
// service that changed, the applied resource limits, the pre-change
// config backup path, and the post-recreate runtime status.
type ReconfigureResult struct {
	// AppID is the app that was reconfigured.
	AppID string `json:"app_id"`

	// Service is the Compose service whose limits changed.
	Service string `json:"service"`

	// ComposeProject is the Compose project whose container was
	// recreated.
	ComposeProject string `json:"compose_project,omitempty"`

	// Memory is the memory limit in effect after the reconfigure.
	Memory string `json:"memory,omitempty"`

	// CPUs is the CPU quota in effect after the reconfigure.
	CPUs string `json:"cpus,omitempty"`

	// PIDs is the pid limit in effect after the reconfigure.
	PIDs int `json:"pids,omitempty"`

	// BackupPath is the pre-change config backup path.
	BackupPath string `json:"backup_path,omitempty"`

	// Status is the post-reconfigure runtime status snapshot.
	Status *AppStatus `json:"status,omitempty"`
}
