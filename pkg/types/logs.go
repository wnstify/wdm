package types

import "time"

// LogsRequest carries the inputs required by Engine.Logs (PRD §24).
// internal/docker Compose wrapper.
type LogsRequest struct {
	// AppID identifies the stack whose logs to stream.
	AppID string `json:"app_id"`

	// Follow streams new log lines as they arrive
	// (the docker compose logs -f equivalent).
	Follow bool `json:"follow,omitempty"`

	// Tail limits the initial output to the last N lines per service.
	// Zero means: stream all available history. Negative is invalid and
	// is rejected by the implementation.
	Tail int `json:"tail,omitempty"`

	// Services optionally restricts streaming to the named services.
	// Nil or empty means: every service in the stack.
	Services []string `json:"services,omitempty"`
}

// LogLine is one structured log entry delivered to a LogLineFn callback.
// PRD does not enumerate fields explicitly; the shape proposed in
type LogLine struct {
	// Timestamp is when the source container emitted the line.
	Timestamp time.Time `json:"timestamp"`

	// AppID identifies the managed stack emitting the line.
	AppID string `json:"app_id,omitempty"`

	// ComposeProject is the Compose project that emitted the line.
	ComposeProject string `json:"compose_project,omitempty"`

	// ContainerName is the concrete container name, when known.
	ContainerName string `json:"container_name,omitempty"`

	// Service is the Compose service name the line originates from.
	Service string `json:"service"`

	// Stream is either "stdout" or "stderr".
	Stream string `json:"stream"`

	// Text is the log message text after timestamp/stream stripping.
	Text string `json:"text"`
}
