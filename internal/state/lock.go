//go:build unix

package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wnstify/wdm/pkg/types"
)

// stackLockSchemaVersion is the schema_version this build
// reads and writes. The constant is the single point of truth; the
// reader compares the on-disk value against it and refuses unknown
// versions with [types.ErrStaleState] rather than guessing at
// migration semantics (PRD §26).
const stackLockSchemaVersion = 1

// stackLockFilename is the well-known marker filename inside every
// managed stack directory. anchors it at
// "<base>/<app>/.wdm.lock"; PRD §9 mirrors the choice.
const stackLockFilename = ".wdm.lock"

// StackLock is the on-disk JSON shape of <stack>/.wdm.lock per
// lines 243–267 and PRD §9. The lock file plays two roles at
// once: state manifest (the fields below) AND flock target (the fd the
// reader opens is what acquires LOCK_EX).
// Field tags use snake_case so the file reads identically to the
// shape specifies and to what hand-debugging via cat / jq
// expects.
// Defaults and conventions:
//   - LastSuccessfulOperation is a pointer because a nil value is a
//     load-bearing signal: it indicates an interrupted install
//     and feeds 's NeedsAttention rule.
//   - BackupHistory is [json.RawMessage] in — the entry
//     shape is settled by PRD §21 in, and using RawMessage
//     here preserves the bytes verbatim on a future
//     read-modify-write.
type StackLock struct {
	// SchemaVersion is the forward-compat marker; locked to
	// [stackLockSchemaVersion] (= 1) in. The reader refuses
	// any other value with [types.ErrStaleState].
	SchemaVersion int `json:"schema_version"`

	// AppID is the stable catalog identifier (e.g. "vaultwarden")
	// and mirrors [types.AppInfo.AppID].
	AppID string `json:"app_id"`

	// TemplateName is the human-readable template label from the
	// catalog entry that produced the stack.
	TemplateName string `json:"template_name"`

	// TemplateVersion is the catalog template's version at install
	// time; later compared against newer catalog versions during
	// update checks (PRD §20).
	TemplateVersion string `json:"template_version"`

	// CatalogChannel is the channel the stack was installed from
	// (PRD §22).
	CatalogChannel string `json:"catalog_channel"`

	// CatalogVersion is the catalog manifest's version string at
	// install time.
	CatalogVersion string `json:"catalog_version"`

	// StackPath is the absolute directory path of the managed stack.
	// The scanner does not verify it matches the directory the lock
	// was read from.
	StackPath string `json:"stack_path"`

	// SelectedDomain is the user-supplied domain for the stack's
	// primary HTTP entry point (e.g. "vault.example.com"). Empty
	// for domain-less stacks; encoded as omitted when empty.
	SelectedDomain string `json:"selected_domain,omitempty"`

	// LocalPorts records the host ports the stack publishes.
	// The slice is empty for stacks reachable only via a reverse
	// proxy.
	LocalPorts []int `json:"local_ports,omitempty"`

	// ComposeProject is the Compose project name (passed as
	// --project-name to docker compose); conventionally
	// "wdm-<app_id>".
	ComposeProject string `json:"compose_project"`

	// ImagePins lists the per-service image references the stack
	// was installed with. Tag and digest are independently optional
	// — tag-only and digest-only pinning are both supported per
	// PRD §22.
	ImagePins []ImagePin `json:"image_pins,omitempty"`

	// GeneratedFields lists placeholder names whose values were
	// auto-generated at install time (e.g. "DB_PASSWORD"). The
	// values themselves live in the rendered .env, never here.
	GeneratedFields []string `json:"generated_fields,omitempty"`

	// LastSuccessfulOperation is the most recent lifecycle event
	// that completed cleanly. nil means no operation has yet
	// succeeded for this stack — typically an interrupted install
	// Reused verbatim from pkg/types so the
	// types.AppInfo projection in scanner.go costs no copy.
	LastSuccessfulOperation *types.Operation `json:"last_successful_operation"`

	// BackupHistory is the per-stack backup ledger maintained by
	// (PRD §21). Stored as raw JSON so a forward-compatible
	// read-modify-write never loses unknown fields. 's update
	// path appends entries through an unexported internal/core record
	// marshaled to json.RawMessage rather than a shared typed struct,
	// keeping this state package agnostic to the ledger's shape.
	BackupHistory []json.RawMessage `json:"backup_history,omitempty"`

	// RecommendedResources records the stack's catalog-recommended
	// resource totals at install time. Install planning subtracts every
	// existing stack's recommended totals — recommended, not
	// selected — from the host budget so each installed stack keeps
	// headroom for its normal allocation. nil for stacks whose
	// catalog entry declares no resources field; additive schema
	// growth only, so locks without the field parse
	// unchanged.
	RecommendedResources *RecommendedResources `json:"recommended_resources,omitempty"`

	// CompletedServices records the Compose service names that complete
	// by design once they run to success — one-shot init containers that
	// exit 0 (mirrored from the app's catalog entry at install time).
	// Status logic treats these as done rather than needs_attention.
	// nil for stacks installed before the field existed; additive schema
	// growth only, so locks without the field parse unchanged.
	CompletedServices []string `json:"completed_services,omitempty"`
}

// RecommendedResources is the per-stack recommended sizing total
// stored in [StackLock.RecommendedResources] for install-time budget
// arithmetic.
type RecommendedResources struct {
	// MemoryBytes is the sum of the catalog's recommended memory
	// band across the stack's services, in bytes.
	MemoryBytes uint64 `json:"memory_bytes"`

	// CPUs is the sum of the catalog's recommended CPU band across
	// the stack's services, in fractional CPUs.
	CPUs float64 `json:"cpus"`
}

// ImagePin is a single service-to-image binding inside [StackLock].
// Per PRD §22 a pin may carry a tag, a digest, or both — at least
// one of Tag and Digest SHOULD be set, but does not enforce
// the constraint at read time (the catalog and renderer own that
// validation upstream).
type ImagePin struct {
	// Service is the Compose service name (e.g. "app", "db").
	Service string `json:"service"`

	// Image is the image reference without tag or digest
	// (e.g. "vaultwarden/server").
	Image string `json:"image"`

	// Tag is the image tag (e.g. "1.30.1"). Empty when the pin is
	// digest-only.
	Tag string `json:"tag,omitempty"`

	// Digest is the image digest (e.g. "sha256:..."). Empty when
	// the pin is tag-only.
	Digest string `json:"digest,omitempty"`
}

// StackLockHandle owns an acquired per-stack .wdm.lock flock. The fd
// stays open until [StackLockHandle.Release], so callers can hold
// cross-process exclusion across a long operation and persist the
// final manifest through the same inode (PRD §26).
// A handle is NOT safe for concurrent use by multiple goroutines.
// The package provides no synchronization beyond Release's
// idempotence: a single goroutine MUST own the acquire/write/release
// lifecycle.
type StackLockHandle struct {
	path string
	file *os.File
	lock *StackLock
}

// Path returns the absolute path of the .wdm.lock file backing h.
// Stable across the handle's lifetime; returns "" for a nil receiver.
func (h *StackLockHandle) Path() string {
	if h == nil {
		return ""
	}
	return h.path
}

// Lock returns the current in-memory manifest snapshot loaded at
// acquisition time or most recently persisted via [Write]. Returns nil
// for a newly created empty lock file and for a nil receiver.
func (h *StackLockHandle) Lock() *StackLock {
	if h == nil || h.lock == nil {
		return nil
	}
	lockCopy := cloneStackLock(*h.lock)
	return &lockCopy
}

// Write persists lock through the held file descriptor using the
// lock-file-safe in-place protocol: Truncate(0), Seek(0), Write, Sync.
// tmp+rename is intentionally forbidden for .wdm.lock because rename
// detaches the path from the inode currently protected by flock
// The held fd is the only valid mutation channel.
func (h *StackLockHandle) Write(lock StackLock) error {
	if h == nil {
		return fmt.Errorf("state.StackLockHandle.Write: nil handle")
	}
	if h.file == nil {
		return fmt.Errorf("state.StackLockHandle.Write: handle at %q is already released", h.path)
	}
	if lock.SchemaVersion != stackLockSchemaVersion {
		return fmt.Errorf(
			"state.StackLockHandle.Write %q: schema_version %d not supported (this build understands %d)",
			h.path, lock.SchemaVersion, stackLockSchemaVersion,
		)
	}

	if err := writeStackLockThroughHeldFile(h.file, lock); err != nil {
		return fmt.Errorf("state.StackLockHandle.Write %q: %w", h.path, err)
	}

	lockCopy := cloneStackLock(lock)
	h.lock = &lockCopy
	return nil
}

// writableFile is the subset of [*os.File] the in-place lock-write protocol
// needs. It is satisfied by [*os.File]; the named interface keeps the
// migration framework's persist call testable without a real fd while
// pinning the held-fd contract (no Open/Close — the caller owns the fd's
// lifecycle).
type writableFile interface {
	Truncate(size int64) error
	Seek(offset int64, whence int) (int64, error)
	Write(p []byte) (int, error)
	Sync() error
}

// writeStackLockThroughHeldFile persists lock to f using the lock-file-safe
// in-place protocol (Truncate(0), Seek(0), Write, Sync). It is the single
// implementation shared by [StackLockHandle.Write] and the migration
// framework's commit point, so the two paths cannot drift. Callers own the
// fd and its flock; this function never opens, closes, or re-locks f.
func writeStackLockThroughHeldFile(f writableFile, lock StackLock) error {
	raw, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	written, err := f.Write(raw)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if written != len(raw) {
		return fmt.Errorf("write: %w", io.ErrShortWrite)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	return nil
}

// Release explicitly unlocks and then closes the underlying file
// descriptor. The explicit unlock makes the release observable before
// the fd teardown path, while close remains the kernel safety net;
// the .wdm.lock file itself remains on disk for subsequent
// acquisitions.
// Release is idempotent: calling it again after the first success is a
// no-op.
func (h *StackLockHandle) Release() error {
	if h == nil || h.file == nil {
		return nil
	}
	unlockErr := Unlock(h.file)
	closeErr := h.file.Close()
	h.file = nil
	var releaseErr error
	if unlockErr != nil {
		releaseErr = errors.Join(
			releaseErr,
			fmt.Errorf("state.StackLockHandle.Release: unlocking %q: %w", h.path, unlockErr),
		)
	}
	if closeErr != nil {
		releaseErr = errors.Join(
			releaseErr,
			fmt.Errorf("state.StackLockHandle.Release: closing %q: %w", h.path, closeErr),
		)
	}
	return releaseErr
}

// AcquireStackLock opens path, creating it with mode 0o600 when absent,
// takes a blocking flock(LOCK_EX), reads the current JSON from the held
// fd, and returns a [*StackLockHandle].
// Behavior by on-disk state:
//   - missing file: created and treated as "no current manifest"
//     (handle.Lock returns nil until Write is called)
//   - existing empty/corrupt/future-schema file: wraps [types.ErrStaleState]
//   - existing valid current-schema file: parsed and exposed via
//     handle.Lock
//   - existing valid OLDER-schema file: backed up, migrated through the
//     held flock, and exposed via handle.Lock at the current schema (the
//     PRD §30 migration framework). A migration failure wraps
//     [types.ErrCodeMigrationFailure] and leaves the on-disk lock untouched.
//
// Because the migration runs under the held exclusive flock and writes
// through the held fd, it is the only entry point that may upgrade a lock —
// the read-only readers ([ReadStackLock], [TryReadStackLock]) keep refusing
// non-current versions with [types.ErrStaleState] (PRD §26 read-only
// clause). Supply [WithMigrationLogger] to record migrations (PRD §30).
// path MUST be absolute; ctx is honored at entry only.
func AcquireStackLock(
	ctx context.Context,
	path string,
	opts ...AcquireOption,
) (*StackLockHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("state.AcquireStackLock: %w", err)
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("state.AcquireStackLock: path must be absolute, got %q", path)
	}
	cfg := resolveAcquireConfig(opts)

	f, created, err := openStackLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("state.AcquireStackLock: opening %q: %w", path, err)
	}

	if err := LockExclusive(f); err != nil {
		return nil, errors.Join(
			fmt.Errorf("state.AcquireStackLock: %w", err),
			f.Close(),
		)
	}

	lock, err := readStackLockFromHeldFile(path, f, created, cfg)
	if err != nil {
		return nil, errors.Join(
			err,
			f.Close(),
		)
	}

	return &StackLockHandle{
		path: path,
		file: f,
		lock: lock,
	}, nil
}

func openStackLockFile(path string) (*os.File, bool, error) {
	// G304 is suppressed: path is composed by internal callers under
	// the engine-controlled stack base, and absolute-path validation
	// happens in AcquireStackLock before this call.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: engine-controlled stack path validated by caller
	if err == nil {
		return f, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}

	// G304 is suppressed: same rationale as above; path origin and
	// absolute validation are owned by AcquireStackLock.
	f, err = os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // G304: engine-controlled stack path validated by caller
	if err != nil {
		return nil, false, err
	}

	return f, false, nil
}

func readStackLockFromHeldFile(
	path string,
	f *os.File,
	created bool,
	cfg acquireConfig,
) (*StackLock, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("state.AcquireStackLock: seeking %q: %w", path, err)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("state.AcquireStackLock: reading %q: %w", path, err)
	}

	if len(raw) == 0 && created {
		return nil, nil
	}

	// Peek at schema_version without committing to a full decode. An older
	// migratable version is routed through the migration framework; the
	// current version and every error shape (corrupt JSON, future version,
	// or an object that omits schema_version or sets it to null) keep
	// decodeStackLock's behavior so read and write paths agree on corruption
	// (PRD §30 migrates only OLDER versions; everything else stays
	// ErrStaleState — a field-less object is never adopted as schema 0).
	version, ok := peekStackLockSchemaVersion(raw)
	if ok && version < stackLockSchemaVersion {
		stackPath := filepath.Dir(path)
		return migrateOlderStackLock(stackPath, path, f, version, raw, cfg.logger)
	}

	return decodeStackLock("state.AcquireStackLock", path, raw)
}

// peekStackLockSchemaVersion extracts schema_version from raw without
// validating the rest of the manifest. ok is false when the bytes do not
// decode as a JSON object carrying an integer schema_version — including an
// object that omits the field or sets it to null. Those shapes fall through to
// decodeStackLock, which maps them onto [types.ErrStaleState] exactly as today;
// a field-less object must NOT be silently adopted by the migration framework
// (behavior preservation — corrupt stays corrupt, never schema 0).
func peekStackLockSchemaVersion(raw []byte) (version int, ok bool) {
	var probe struct {
		// Pointer so an absent field or an explicit JSON null both decode to
		// nil, distinguishing them from a present integer 0.
		SchemaVersion *int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.SchemaVersion == nil {
		return 0, false
	}
	return *probe.SchemaVersion, true
}

// decodeStackLock parses raw.wdm.lock bytes into a fresh [*StackLock],
// mapping the three corrupt-manifest shapes — empty file, JSON decode
// failure, unsupported schema_version — onto [types.ErrStaleState] with
// the caller's operation name as the message prefix. The shared decode
// keeps the reader entry points ([ReadStackLock], [TryReadStackLock],
// [AcquireStackLock]) byte-identical in their corruption semantics.
func decodeStackLock(op, path string, raw []byte) (*StackLock, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf(
			"%s %q: %w: file is empty (interrupted write?)",
			op, path, types.ErrStaleState,
		)
	}

	var lock StackLock
	if err := json.Unmarshal(raw, &lock); err != nil {
		return nil, fmt.Errorf(
			"%s %q: %w: json decode: %w",
			op, path, types.ErrStaleState, err,
		)
	}

	if lock.SchemaVersion != stackLockSchemaVersion {
		return nil, fmt.Errorf(
			"%s %q: %w: schema_version %d not supported (this build understands %d)",
			op, path, types.ErrStaleState, lock.SchemaVersion, stackLockSchemaVersion,
		)
	}

	return &lock, nil
}

func cloneStackLock(lock StackLock) StackLock {
	clone := lock
	clone.LocalPorts = append([]int(nil), lock.LocalPorts...)
	clone.ImagePins = append([]ImagePin(nil), lock.ImagePins...)
	clone.GeneratedFields = append([]string(nil), lock.GeneratedFields...)
	clone.CompletedServices = append([]string(nil), lock.CompletedServices...)
	if lock.LastSuccessfulOperation != nil {
		op := *lock.LastSuccessfulOperation
		clone.LastSuccessfulOperation = &op
	}
	if lock.RecommendedResources != nil {
		resources := *lock.RecommendedResources
		clone.RecommendedResources = &resources
	}
	clone.BackupHistory = make([]json.RawMessage, len(lock.BackupHistory))
	for i, entry := range lock.BackupHistory {
		clone.BackupHistory[i] = append(json.RawMessage(nil), entry...)
	}
	return clone
}

// ReadStackLock opens path with O_RDONLY, takes a blocking exclusive
// flock (LOCK_EX) on the fd, reads the JSON, releases the flock by
// closing the fd, and returns the parsed [*StackLock].
// Wrapping semantics:
//   - empty file, JSON parse failure, and schema_version mismatch
//     all wrap [types.ErrStaleState] so [errors.Is] matches the
//     sentinel; cmd/wdm maps it to a stale-state hint message.
//   - missing file is returned as a wrapped [os.ErrNotExist] (NOT
//     ErrStaleState) so the scanner can distinguish "user-owned
//     directory" from "corrupt managed stack" — they are different
//     user-facing situations.
//   - other I/O errors (EACCES, EIO) propagate unwrapped beneath
//     the "state.ReadStackLock:" prefix.
//
// path MUST be absolute; relative paths are rejected up front. Path
// expansion (~/ → $HOME) is upstream of this function per
// "On-disk layout".
// flock(LOCK_EX) is BLOCKING here — concurrent readers/writers wait
// rather than fail, mirroring PRD §26's read-modify-write protocol.
// The runtime.lock takes the non-blocking path; the per-stack lock
// does not, because a contended stack is expected during a concurrent
// update and blocking is the user-friendly outcome.
// ctx is honored at entry only; the syscalls below are local and fast,
// so finer-grained cancellation would add complexity without changing
// observable behavior.
func ReadStackLock(ctx context.Context, path string) (*StackLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("state.ReadStackLock: %w", err)
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("state.ReadStackLock: path must be absolute, got %q", path)
	}

	// G304 is suppressed: path is composed by the scanner under the
	// engine-controlled base stack path,
	// and the absolute-path check above forecloses relative
	// re-injection. Same engine-XDG-path rationale as
	// runtime_lock.go and config.go.
	f, err := os.OpenFile(path, os.O_RDONLY, 0) //nolint:gosec // G304: path composed under engine-controlled stack base
	if err != nil {
		return nil, fmt.Errorf("state.ReadStackLock: opening %q: %w", path, err)
	}

	if err := LockExclusive(f); err != nil {
		return nil, errors.Join(
			fmt.Errorf("state.ReadStackLock: %w", err),
			f.Close(),
		)
	}

	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("state.ReadStackLock: reading %q: %w", path, err),
			f.Close(),
		)
	}
	// Closing the last fd releases the flock; no separate Unlock
	// needed. The kernel guarantee is documented in flock.go.
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("state.ReadStackLock: closing %q: %w", path, err)
	}

	return decodeStackLock("state.ReadStackLock", path, raw)
}

// ErrStackLockBusy is the sentinel returned (wrapped) by
// [TryReadStackLock] when another process holds the per-stack
// .wdm.lock flock — i.e. a state-changing operation is actively
// working on the stack. Detect with [errors.Is].
// internal/core's read-only Status path maps this onto
// [types.ErrCodeRuntimeLockHeld] so the user sees the PRD §26
// "another operation is already running" outcome instead of an
// indefinite stall behind the writer's flock.
var ErrStackLockBusy = errors.New("state: stack lock held by another process")

// TryReadStackLock is the non-blocking, read-only sibling of
// [ReadStackLock]: it opens path with O_RDONLY, attempts a SHARED
// flock without blocking (LOCK_SH | LOCK_NB via [TryLockShared]),
// reads and parses the JSON under the held lock, and releases the
// flock by closing the fd.
// Contention semantics differ deliberately from [ReadStackLock]:
//   - [ReadStackLock] blocks on LOCK_EX because its callers (the
//     scanner, write-path preparation) already operate inside the
//     PRD §26 read-modify-write protocol where waiting is the
//     user-friendly outcome.
//   - TryReadStackLock NEVER waits. PRD §26 allows read-only
//     commands, such as status checks, "only when they cannot
//     conflict with the active operation" — when a writer holds the
//     per-stack flock, a status read WOULD conflict (it would stall
//     for the duration of the operation, or observe a manifest
//     mid-truncate under the in-place write pattern), so the
//     attempt fails fast with a wrapped [ErrStackLockBusy] instead.
//
// The shared (not exclusive) lock mode means concurrent
// TryReadStackLock callers never contend with each other; only a
// writer's LOCK_EX produces the busy outcome.
// All other semantics mirror [ReadStackLock]: corrupt manifests wrap
// [types.ErrStaleState], a missing file wraps [os.ErrNotExist], path
// MUST be absolute, and ctx is honored at entry only.
func TryReadStackLock(ctx context.Context, path string) (*StackLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("state.TryReadStackLock: %w", err)
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("state.TryReadStackLock: path must be absolute, got %q", path)
	}

	// G304 is suppressed: path is composed by internal/core under the
	// engine-controlled stack base, and the absolute-path check above
	// forecloses relative re-injection. Same rationale as
	// ReadStackLock.
	f, err := os.OpenFile(path, os.O_RDONLY, 0) //nolint:gosec // G304: path composed under engine-controlled stack base
	if err != nil {
		return nil, fmt.Errorf("state.TryReadStackLock: opening %q: %w", path, err)
	}

	got, err := TryLockShared(f)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("state.TryReadStackLock: %w", err),
			f.Close(),
		)
	}
	if !got {
		return nil, errors.Join(
			fmt.Errorf("state.TryReadStackLock %q: %w", path, ErrStackLockBusy),
			f.Close(),
		)
	}

	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("state.TryReadStackLock: reading %q: %w", path, err),
			f.Close(),
		)
	}
	// Closing the last fd releases the flock; no separate Unlock
	// needed. The kernel guarantee is documented in flock.go.
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("state.TryReadStackLock: closing %q: %w", path, err)
	}

	return decodeStackLock("state.TryReadStackLock", path, raw)
}
