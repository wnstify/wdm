package types

import (
	"errors"
	"fmt"
)

// ErrorCode maps directly to wdm's CLI exit codes (PRD §27). Each value
// is the integer the cmd/wdm entry point passes to os.Exit when an
// error of this code surfaces.
// The zero value (ErrCodeUnknown) is reserved as a "this was never set"
// sentinel and must never appear in a returned *Error — it would
// otherwise be mapped to the success exit code (0), silently masking a
// real failure.
type ErrorCode int

// Exit-code-bearing error codes. Order and numeric values are fixed by
// PRD §27; the cmd/wdm entry point relies on int(code) yielding the
// correct exit status.
const (
	// ErrCodeUnknown is the zero value; it indicates a programming error
	// (code field was never set) and must never reach cmd/wdm.
	ErrCodeUnknown ErrorCode = 0

	// ErrCodeGeneric is a catch-all for failures that do not fit any of
	// the categories below.
	ErrCodeGeneric ErrorCode = 1

	// ErrCodeUsageValidation indicates malformed user input or a config
	// schema violation.
	ErrCodeUsageValidation ErrorCode = 2

	// ErrCodeVerificationFailed indicates a release, catalog, or
	// signature verification failure.
	ErrCodeVerificationFailed ErrorCode = 3

	// ErrCodeRuntimeLockHeld indicates another wdm process is already
	// holding the runtime lock.
	ErrCodeRuntimeLockHeld ErrorCode = 4

	// ErrCodeDockerUnavailable indicates Docker or the Compose plugin is
	// missing or not reachable.
	ErrCodeDockerUnavailable ErrorCode = 5

	// ErrCodePermissionDenied indicates the binary was invoked as root or
	// via sudo, or a filesystem permission check failed.
	ErrCodePermissionDenied ErrorCode = 6

	// ErrCodeUserCanceled indicates the user dismissed a Confirmer
	// prompt or sent SIGINT during an operation.
	ErrCodeUserCanceled ErrorCode = 7

	// ErrCodeNetworkFailure indicates a registry, GitHub, or catalog
	// download failure.
	ErrCodeNetworkFailure ErrorCode = 8

	// ErrCodeMigrationFailure indicates a schema or state migration step
	// failed.
	ErrCodeMigrationFailure ErrorCode = 9
)

// String returns a short, lowercase identifier suitable for logs and the
// "code" field of the JSON envelope. Format is stable across releases.
func (c ErrorCode) String() string {
	switch c {
	case ErrCodeUnknown:
		return "unknown"
	case ErrCodeGeneric:
		return "generic"
	case ErrCodeUsageValidation:
		return "usage_validation"
	case ErrCodeVerificationFailed:
		return "verification_failed"
	case ErrCodeRuntimeLockHeld:
		return "runtime_lock_held"
	case ErrCodeDockerUnavailable:
		return "docker_unavailable"
	case ErrCodePermissionDenied:
		return "permission_denied"
	case ErrCodeUserCanceled:
		return "user_canceled"
	case ErrCodeNetworkFailure:
		return "network_failure"
	case ErrCodeMigrationFailure:
		return "migration_failure"
	default:
		return fmt.Sprintf("invalid(%d)", int(c))
	}
}

// Error is the JSON-serializable error payload emitted via --json
// (PRD §32, §37). Code maps to the CLI exit code (PRD §27); Message is
// the user-visible summary; Hint suggests a next action (PRD §25) and
// may be empty. Cause carries the underlying error chain for logs and
// errors.Is/As traversal but is never serialized.
type Error struct {
	// Code maps 1:1 to the CLI exit code (PRD §27).
	Code ErrorCode `json:"code"`

	// Message is the short user-visible summary; lowercase, no trailing
	// punctuation, no PII.
	Message string `json:"message"`

	// Hint is an optional next-action suggestion shown after Message.
	Hint string `json:"hint,omitempty"`

	// Cause is the wrapped underlying error, hidden from JSON output so
	// internal details never leak to the wire (PRD §11, §24).
	Cause error `json:"-"`
}

// Compile-time confirmation that *Error satisfies the error interface.
var _ error = &Error{}

// Error implements the error interface. The returned string is intended
// for logs and developer diagnostics; user-visible output should consume
// Message and Hint directly via the JSON envelope.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s [%s]: %s", e.Message, e.Code, e.Cause)
	}
	return fmt.Sprintf("%s [%s]", e.Message, e.Code)
}

// Unwrap exposes Cause to errors.Is and errors.As, letting callers test
// for wrapped sentinels (e.g. errors.Is(err, ErrConfigInvalid)).
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewError builds a typed *Error with no underlying cause.
func NewError(code ErrorCode, message, hint string) *Error {
	return &Error{Code: code, Message: message, Hint: hint}
}

// WrapError builds a typed *Error that wraps cause. Use this when a
// lower-layer error needs to be surfaced with a CLI exit code attached.
func WrapError(code ErrorCode, message, hint string, cause error) *Error {
	return &Error{Code: code, Message: message, Hint: hint, Cause: cause}
}

// IsCode reports whether err (or anything in its chain) is a *Error with
// the given code. Intended for cmd/wdm's exit-code mapping (PRD §27).
func IsCode(err error, code ErrorCode) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code == code
}

// Sentinel errors. Compare with errors.Is; wrap with WrapError to attach
// a CLI exit code and a user-visible message.
var (
	// ErrConfigInvalid is returned by engine.New when the user's
	// config.toml fails schema validation (PRD §34).
	ErrConfigInvalid = errors.New("types: config invalid")

	// ErrStaleState is returned by the state layer when a .wdm.lock
	// file is missing, partial, or fails to parse — typically the result
	// of a crash mid-write (PRD §26).
	ErrStaleState = errors.New("types: stack state is stale or corrupt")
)
