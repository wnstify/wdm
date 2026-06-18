package types

// StopAllRequest carries the inputs for Engine.StopAll, the batch
// "stop all apps" operation (issue #27). StopAll runs `docker compose
// stop` against every managed stack: it stops the running containers
// without removing them, so containers, networks, and named volumes stay
// defined and all data is preserved (this is NOT `docker compose down`).
// The request is intentionally argless: StopAll always targets the full
// managed set discovered under the configured stack base. There is no
// per-app or per-service selector in this version.
type StopAllRequest struct{}

// StoppedApp is one stack's outcome inside a StopAllResult. It names the
// managed stack and, when the stop failed, carries the redacted failure
// detail. The whole-stack `docker compose stop` is idempotent, so an
// already-stopped stack is reported as a success no-op (Error empty).
type StoppedApp struct {
	// AppID is the managed app the outcome describes.
	AppID string `json:"app_id"`

	// ComposeProject is the Compose project whose containers were
	// targeted. It may be empty when the stack manifest was unreadable
	// before the stop could run.
	ComposeProject string `json:"compose_project,omitempty"`

	// Error holds the failure detail when this stack failed to stop. It
	// is empty on success. The message carries no secret values (stack
	// path, Compose project, and the docker-layer reason only).
	Error string `json:"error,omitempty"`
}

// StopAllResult summarizes a completed StopAll run. StopAll is
// continue-on-error: every managed stack is attempted even if some fail,
// so the result partitions the managed set into the stacks that stopped
// and the stacks that failed. An empty managed set yields two empty
// slices and a nil operation error.
type StopAllResult struct {
	// Stopped lists the stacks that stopped cleanly (including already
	// stopped no-ops).
	Stopped []StoppedApp `json:"stopped,omitempty"`

	// Failed lists the stacks whose stop failed, each carrying its
	// redacted failure detail in StoppedApp.Error.
	Failed []StoppedApp `json:"failed,omitempty"`
}
