package types

// PortConflictError reports a plan-time host-port conflict on a remappable
// single loopback (127.0.0.1) binding (ADR 0004 / PRD §11.1). It carries the
// conflicting binding and a deterministic suggested free host port
// (SuggestedHostPort == 0 means none was found, fail-closed), so a caller can
// surface a `--port HOST=NEW` remap path instead of just aborting.
//
// It wraps a ErrCodeUsageValidation *Error: errors.As(err, &*Error) still
// yields the PRD §27 exit code, while errors.As(err, &*PortConflictError)
// recovers the structured detail the CLI envelope reports.
type PortConflictError struct {
	// Service is the catalog service whose host binding conflicts.
	Service string

	// ContainerPort is the in-container port behind the conflicting binding.
	ContainerPort int

	// ConflictingHostPort is the busy 127.0.0.1 host port.
	ConflictingHostPort int

	// SuggestedHostPort is the deterministic next-free host port; 0 means no
	// free port was found within the scan and the caller must fail closed.
	SuggestedHostPort int

	// Err is the underlying usage-validation error carrying the exit code,
	// message, and hint. Never serialized to JSON (see Error.Cause).
	Err *Error
}

// NewPortConflictError builds a typed conflict error from the conflicting
// binding, the deterministic suggestion, and the underlying usage-validation
// error.
func NewPortConflictError(service string, containerPort, conflictingHostPort, suggestedHostPort int, err *Error) *PortConflictError {
	return &PortConflictError{
		Service:             service,
		ContainerPort:       containerPort,
		ConflictingHostPort: conflictingHostPort,
		SuggestedHostPort:   suggestedHostPort,
		Err:                 err,
	}
}

// Error implements the error interface, delegating to the wrapped *Error so the
// message and exit code stay byte-compatible with a plain port conflict.
func (e *PortConflictError) Error() string {
	return e.Err.Error()
}

// Unwrap exposes the wrapped *Error to errors.Is/As, so exit-code mapping and
// sentinel checks traverse the conflict error transparently.
func (e *PortConflictError) Unwrap() error {
	return e.Err
}
