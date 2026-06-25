package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

type installFileWrite struct {
	path string
	data []byte
	mode os.FileMode
}

// writeInstallFiles refuses existing managed or unmanaged stack
// paths, creates the fresh stack directory, acquires the per-stack
// .wdm.lock flock, and writes the rendered artifacts atomically. On
// success it returns the HELD lock handle so the caller keeps the
// flock across confirm → networks → deploy → manifest write → release
// A fault after the stack directory is
// created triggers the fresh-install sad-path cleanup before the error
// returns.
func writeInstallFiles(ctx context.Context, plan *installPlan, onProgress types.ProgressFn) (*state.StackLockHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	writes, err := installFileWrites(plan)
	if err != nil {
		return nil, err
	}

	stackExists, err := installStackPathExists(plan.stackPath)
	if err != nil {
		return nil, err
	}
	if stackExists {
		handle, err := acquireExistingInstallStackLock(ctx, plan.stackPath)
		if err != nil {
			return nil, err
		}
		defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

		if handle.Lock() != nil {
			return nil, managedStackExistsError(plan.stackPath)
		}
		return nil, stackPathExistsError(plan.stackPath)
	}

	if err := createInstallStackDir(plan.stackPath); err != nil {
		return nil, err
	}

	handle, err := acquireInstallStackLock(ctx, plan.stackPath)
	if err != nil {
		return nil, failFreshInstall(ctx, err, plan, nil, nil)
	}
	if handle.Lock() != nil {
		// A foreign manifest appeared between mkdir and flock. Refuse
		// WITHOUT cleanup: removing another actor's manifest would
		// destroy state wdm does not own.
		if releaseErr := handle.Release(); releaseErr != nil {
			return nil, errors.Join(managedStackExistsError(plan.stackPath), releaseErr)
		}
		return nil, managedStackExistsError(plan.stackPath)
	}

	if onProgress != nil {
		onProgress(types.StepInstallWriteFiles, 35, "writing install files")
	}
	for _, write := range writes {
		if err := validateInstallWritePath(plan.stackPath, write.path); err != nil {
			return nil, failFreshInstall(ctx, usageValidationError(
				"install file path is unsafe",
				"remove symlinks from the stack path and retry",
				err,
			), plan, handle, nil)
		}
		if err := state.WriteFileAtomic(write.path, write.data, write.mode); err != nil {
			return nil, failFreshInstall(ctx, types.WrapError(
				types.ErrCodeGeneric,
				"install files could not be written",
				"check stack directory permissions and retry",
				err,
			), plan, handle, nil)
		}
	}
	return handle, nil
}

func installFileWrites(plan *installPlan) ([]installFileWrite, error) {
	if plan == nil {
		return nil, types.NewError(
			types.ErrCodeUsageValidation,
			"install plan is required",
			"construct an install plan before writing files",
		)
	}

	composePath, err := security.SafeJoin(plan.stackPath, installComposeFilename)
	if err != nil {
		return nil, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	envPath, err := security.SafeJoin(plan.stackPath, installEnvFilename)
	if err != nil {
		return nil, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}

	writes := []installFileWrite{
		{
			path: composePath,
			data: plan.rendered.ComposeBytes,
			mode: installComposeFileMode,
		},
		{
			path: envPath,
			data: plan.rendered.EnvBytes,
			mode: security.SecretFileMode,
		},
	}

	artifactWrites, err := renderedArtifactWrites(&plan.rendered, plan.stackPath)
	if err != nil {
		return nil, err
	}
	writes = append(writes, artifactWrites...)
	return writes, nil
}

// ensureUserEnvFile seeds an empty user-owned .env.user (0600) inside
// stackPath only when it is absent, and returns its resolved path. The
// file is user-editable env injected into every service via the
// template env_file: directive; wdm creates it but NEVER regenerates or
// truncates it, so install, edit, and rewire all share this primitive
// while `wdm update` leaves the user's content untouched.
// security.CreateSecretFile's O_EXCL makes the create idempotent: an
// already-present file surfaces fs.ErrExist, which is treated as "kept
// as-is" rather than an error. The returned file is empty and closed.
func ensureUserEnvFile(stackPath string) (string, error) {
	path, err := security.SafeJoin(stackPath, installEnvUserFilename)
	if err != nil {
		return "", usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	f, err := security.CreateSecretFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return path, nil
		}
		return "", err
	}
	if closeErr := f.Close(); closeErr != nil {
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"user env file could not be finalized",
			"check stack directory permissions and retry",
			closeErr,
		)
	}
	return path, nil
}

// renderedArtifactWrites enumerates the rendered additional_files and
// config_artifacts as concrete file writes rooted at root, in the stable
// order additional_files-then-config_artifacts. It is the single source
// of truth for which sidecar artifacts a stack deploys: the real install
// writer roots it at the stack path, and the pre-deploy compose-config
// validation roots it at its hermetic temp project dir, so the validated
// file set can never drift from the deployed file set.
// One [installAdditionalDestTracker] spans both kinds so a config
// artifact cannot collide with an additional file, a reserved file, its
// temp path, or another artifact of either kind; the kind threads into
// every diagnostic so a rejected config artifact is reported accurately.
// Every Dest is path-safe-joined against root and its mode parsed at the
// filesystem boundary, exactly as the deploy writer requires.
func renderedArtifactWrites(rendered *render.RenderedStack, root string) ([]installFileWrite, error) {
	if rendered == nil {
		return nil, types.NewError(
			types.ErrCodeUsageValidation,
			"rendered stack is required",
			"render the stack before enumerating artifact writes",
		)
	}

	cleanRoot := filepath.Clean(root)
	tracker := newInstallAdditionalDestTracker()
	writes := make([]installFileWrite, 0, len(rendered.AdditionalFiles)+len(rendered.ConfigArtifacts))

	add := func(kind, unsafeMsg, reservedMsg, modeMsg string, file render.RenderedFile) error {
		path, err := security.SafeJoin(root, file.Dest)
		if err != nil {
			return catalogVerificationError(unsafeMsg, "refresh the catalog and retry", err)
		}
		relPath, err := filepath.Rel(cleanRoot, filepath.Clean(path))
		if err != nil {
			return catalogVerificationError(unsafeMsg, "refresh the catalog and retry", err)
		}
		if err := tracker.add(kind, file.Dest, relPath); err != nil {
			return catalogVerificationError(reservedMsg, "refresh the catalog and retry", err)
		}
		mode, err := parseRenderedFileMode(file.Mode)
		if err != nil {
			return catalogVerificationError(modeMsg, "refresh the catalog and retry", err)
		}
		writes = append(writes, installFileWrite{
			path: path,
			data: file.Bytes,
			mode: mode,
		})
		return nil
	}

	for _, file := range rendered.AdditionalFiles {
		if err := add(
			installAdditionalFileKind,
			"catalog additional file destination is unsafe",
			"catalog additional file destination is reserved or duplicated",
			"catalog additional file mode is invalid",
			file,
		); err != nil {
			return nil, err
		}
	}
	for _, artifact := range rendered.ConfigArtifacts {
		if err := add(
			installConfigArtifactKind,
			"catalog config artifact destination is unsafe",
			"catalog config artifact destination is reserved or duplicated",
			"catalog config artifact mode is invalid",
			artifact,
		); err != nil {
			return nil, err
		}
	}
	return writes, nil
}

type installAdditionalDestTracker struct {
	final map[string]string
	temp  map[string]string
}

func newInstallAdditionalDestTracker() *installAdditionalDestTracker {
	return &installAdditionalDestTracker{
		final: map[string]string{
			installComposeFilename:         installComposeFilename,
			installEnvFilename:             installEnvFilename,
			installEnvUserFilename:         installEnvUserFilename,
			installComposeOverrideFilename: installComposeOverrideFilename,
			installLockFilename:            installLockFilename,
			state.BackupDirName:            state.BackupDirName,
		},
		temp: map[string]string{
			installComposeFilename + ".tmp": installComposeFilename,
			installEnvFilename + ".tmp":     installEnvFilename,
			installLockFilename + ".tmp":    installLockFilename,
		},
	}
}

// add records a stack-relative destination for an artifact of the named
// kind ("additional file" or "config artifact") and refuses any
// collision with a reserved file, its temp path, or an already-recorded
// artifact of either kind. The kind threads into every diagnostic so a
// rejected config artifact is never misreported as an additional file;
// it does not affect which paths conflict, so both kinds share one
// reserved-name set and dest namespace.
func (t *installAdditionalDestTracker) add(kind string, rawDest string, relPath string) error {
	cleaned := filepath.Clean(relPath)
	if cleaned == "." {
		return fmt.Errorf("%s %q targets the stack root", kind, rawDest)
	}
	if existing, owner, ok := t.findConflict(cleaned, t.final); ok {
		return fmt.Errorf(
			"%s %q targets %q that conflicts with %s at %q",
			kind,
			rawDest,
			cleaned,
			owner,
			existing,
		)
	}
	if existing, owner, ok := t.findConflict(cleaned, t.temp); ok {
		return fmt.Errorf(
			"%s %q targets %q that conflicts with temp path for %s at %q",
			kind,
			rawDest,
			cleaned,
			owner,
			existing,
		)
	}

	tempPath := cleaned + ".tmp"
	if existing, owner, ok := t.findConflict(tempPath, t.final); ok {
		return fmt.Errorf(
			"%s %q would use temp path %q that conflicts with %s at %q",
			kind,
			rawDest,
			tempPath,
			owner,
			existing,
		)
	}
	if existing, owner, ok := t.findConflict(tempPath, t.temp); ok {
		return fmt.Errorf(
			"%s %q would use temp path %q that conflicts with temp path for %s at %q",
			kind,
			rawDest,
			tempPath,
			owner,
			existing,
		)
	}

	owner := fmt.Sprintf("%s %q", kind, rawDest)
	t.final[cleaned] = owner
	t.temp[tempPath] = owner
	return nil
}

func (t *installAdditionalDestTracker) findConflict(candidate string, paths map[string]string) (string, string, bool) {
	for existing, owner := range paths {
		if installPathsConflict(candidate, existing) {
			return existing, owner, true
		}
	}
	return "", "", false
}

func installPathsConflict(a, b string) bool {
	return installPathHasRoot(a, b) || installPathHasRoot(b, a)
}

func installPathHasRoot(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func installStackPathExists(stackPath string) (bool, error) {
	if err := security.RejectUnsafeRoot(stackPath); err != nil {
		return false, stackPathUnsafeError(err)
	}
	if err := validateInstallPathAncestors(stackPath); err != nil {
		return false, stackPathUnsafeError(err)
	}

	info, err := os.Lstat(stackPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, types.WrapError(
			types.ErrCodeGeneric,
			"stack directory could not be inspected",
			"check stack directory permissions and retry",
			err,
		)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, stackPathUnsafeError(fmt.Errorf("stack path %q is a symlink", stackPath))
	}
	if !info.IsDir() {
		return false, usageValidationError(
			"stack path is not a directory",
			"choose an empty directory path for the stack",
			fmt.Errorf("stack path %q is not a directory", stackPath),
		)
	}
	return true, nil
}

func createInstallStackDir(stackPath string) error {
	if err := security.RejectUnsafeRoot(stackPath); err != nil {
		return stackPathUnsafeError(err)
	}
	if err := validateInstallPathAncestors(stackPath); err != nil {
		return stackPathUnsafeError(err)
	}

	parent := filepath.Dir(stackPath)
	if err := os.MkdirAll(parent, state.GeneratedDirMode); err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack parent directories could not be created",
			"check stack directory permissions and retry",
			err,
		)
	}
	if err := validateInstallPathAncestors(stackPath); err != nil {
		return stackPathUnsafeError(err)
	}
	if err := os.Mkdir(stackPath, state.GeneratedDirMode); err != nil {
		if errors.Is(err, os.ErrExist) {
			return stackPathExistsError(stackPath)
		}
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack directory could not be created",
			"check stack directory permissions and retry",
			err,
		)
	}
	if err := os.Chmod(stackPath, state.GeneratedDirMode); err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack directory mode could not be enforced",
			"check stack directory permissions and retry",
			err,
		)
	}
	if err := state.SyncDirectory(parent); err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack directory entry could not be synced",
			"check stack directory permissions and retry",
			err,
		)
	}
	return nil
}

func acquireExistingInstallStackLock(ctx context.Context, stackPath string) (*state.StackLockHandle, error) {
	lockPath := filepath.Join(stackPath, ".wdm.lock")
	info, err := os.Lstat(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, stackPathExistsError(stackPath)
	}
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"stack lock could not be inspected",
			"check stack directory permissions and retry",
			err,
		)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, stackPathUnsafeError(fmt.Errorf("stack lock %q is a symlink", lockPath))
	}
	if !info.Mode().IsRegular() {
		return nil, usageValidationError(
			"stack lock is not a regular file",
			"choose a different stack path or remove the existing stack first",
			fmt.Errorf("stack lock %q is not a regular file", lockPath),
		)
	}
	return acquireInstallStackLock(ctx, stackPath)
}

func acquireInstallStackLock(ctx context.Context, stackPath string) (*state.StackLockHandle, error) {
	handle, err := state.AcquireStackLock(ctx, filepath.Join(stackPath, ".wdm.lock"))
	if err != nil {
		return nil, fmt.Errorf("core.install: acquiring stack lock: %w", err)
	}
	return handle, nil
}

func validateInstallWritePath(stackPath, targetPath string) error {
	if err := security.EnsureWithinRoot(filepath.Clean(stackPath), filepath.Clean(targetPath)); err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Clean(stackPath), filepath.Clean(targetPath))
	if err != nil {
		return fmt.Errorf("calculating relative path for %q: %w", targetPath, err)
	}
	return validateInstallRelativePathAncestors(stackPath, rel)
}

func validateInstallRelativePathAncestors(stackPath, relativePath string) error {
	parentPath := filepath.Dir(filepath.Clean(relativePath))
	if parentPath == "." {
		return nil
	}

	currentPath := filepath.Clean(stackPath)
	for _, component := range strings.Split(parentPath, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}

		currentPath = filepath.Join(currentPath, component)
		info, err := os.Lstat(currentPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stating install file path component %q: %w", currentPath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("install file path component %q is a symlink", currentPath)
		}
		if !info.IsDir() {
			return fmt.Errorf("install file path component %q is not a directory", currentPath)
		}
	}
	return nil
}

func validateInstallPathAncestors(path string) error {
	parentPath := filepath.Dir(filepath.Clean(path))
	if parentPath == "." || parentPath == string(filepath.Separator) {
		return nil
	}

	currentPath := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(parentPath, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}

		currentPath = filepath.Join(currentPath, component)
		info, err := os.Lstat(currentPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stating stack path component %q: %w", currentPath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("stack path component %q is a symlink", currentPath)
		}
		if !info.IsDir() {
			return fmt.Errorf("stack path component %q is not a directory", currentPath)
		}
	}
	return nil
}

func stackPathUnsafeError(cause error) error {
	return usageValidationError(
		"stack path is unsafe",
		"choose a stack path under your home directory without symlinked path components",
		cause,
	)
}

func stackPathExistsError(stackPath string) error {
	return usageValidationError(
		"stack path already exists",
		"choose an empty stack path or remove the existing directory first",
		fmt.Errorf("stack path %q already exists", stackPath),
	)
}

func managedStackExistsError(stackPath string) error {
	return usageValidationError(
		"stack is already managed",
		"choose a different stack path or remove the existing stack first",
		fmt.Errorf("stack %q already has a lock manifest", stackPath),
	)
}

func parseRenderedFileMode(mode string) (os.FileMode, error) {
	parsed, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal: %w", mode, err)
	}
	if parsed > 0o777 {
		return 0, fmt.Errorf("mode %q is outside permission bits", mode)
	}
	return os.FileMode(parsed), nil
}
