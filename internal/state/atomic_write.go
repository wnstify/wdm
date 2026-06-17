//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wnstify/wdm/internal/security"
)

// GeneratedDirMode is the permission mode for any parent directory this
// package auto-creates while preparing atomic writes.
const GeneratedDirMode os.FileMode = 0o755

// SyncDirectory opens path, fsyncs it, and closes it.
// The path MUST be absolute and MUST point to a directory.
func SyncDirectory(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("state.SyncDirectory: path must be absolute, got %q", path)
	}

	// G304 is suppressed: callers pass engine-controlled absolute paths.
	// The absolute-path guard above forecloses relative-path re-injection.
	dir, err := os.Open(path) //nolint:gosec // G304: absolute path is validated; caller controls lifecycle paths
	if err != nil {
		return fmt.Errorf("state.SyncDirectory: opening directory %q: %w", path, err)
	}

	info, err := dir.Stat()
	if err != nil {
		return errors.Join(
			fmt.Errorf("state.SyncDirectory: stating directory %q: %w", path, err),
			dir.Close(),
		)
	}
	if !info.IsDir() {
		return errors.Join(
			fmt.Errorf("state.SyncDirectory: %q is not a directory", path),
			dir.Close(),
		)
	}
	if err := dir.Sync(); err != nil {
		return errors.Join(
			fmt.Errorf("state.SyncDirectory: fsync directory %q: %w", path, err),
			dir.Close(),
		)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("state.SyncDirectory: closing directory %q: %w", path, err)
	}
	return nil
}

// WriteFileAtomic writes data to path via temp-file + fsync + rename.
// The path MUST be absolute. Parent directories are created on demand at
// [GeneratedDirMode]; each created directory entry is fsync'd in its parent
// so nested creations are durable.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("state.WriteFileAtomic: path must be absolute, got %q", path)
	}

	parent := filepath.Dir(path)
	if err := ensureParentDirectories(parent); err != nil {
		return fmt.Errorf("state.WriteFileAtomic: preparing parents for %q: %w", path, err)
	}
	if mode == security.SecretFileMode {
		if err := security.RejectInsecureParent(path); err != nil {
			return fmt.Errorf("state.WriteFileAtomic: validating parent security for %q: %w", path, err)
		}
	}

	tempPath := path + ".tmp"
	tempFile, err := createTempFileForAtomicWrite(tempPath, mode)
	if err != nil {
		return errors.Join(
			fmt.Errorf("state.WriteFileAtomic: creating temp file %q: %w", tempPath, err),
			removeTempFileIfPresent(tempPath),
		)
	}

	if err := writeAllAndSync(tempFile, tempPath, data); err != nil {
		return errors.Join(
			err,
			closeAndRemoveTempFile(tempFile, tempPath),
		)
	}
	if err := tempFile.Close(); err != nil {
		return errors.Join(
			fmt.Errorf("state.WriteFileAtomic: closing temp file %q: %w", tempPath, err),
			removeTempFileIfPresent(tempPath),
		)
	}

	if err := os.Rename(tempPath, path); err != nil {
		return errors.Join(
			fmt.Errorf("state.WriteFileAtomic: renaming %q to %q: %w", tempPath, path, err),
			removeTempFileIfPresent(tempPath),
		)
	}

	if err := validateFinalFileMode(path, mode); err != nil {
		return err
	}
	if err := SyncDirectory(parent); err != nil {
		return fmt.Errorf("state.WriteFileAtomic: syncing parent directory %q: %w", parent, err)
	}
	return nil
}

func ensureParentDirectories(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("parent path must be absolute, got %q", path)
	}

	missing := make([]string, 0, 4)
	cursor := path
	for {
		info, err := os.Stat(cursor)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("path component %q is not a directory", cursor)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stating %q: %w", cursor, err)
		}
		missing = append(missing, cursor)

		next := filepath.Dir(cursor)
		if next == cursor {
			return fmt.Errorf("no existing ancestor found for %q", path)
		}
		cursor = next
	}

	for i := len(missing) - 1; i >= 0; i-- {
		dirPath := missing[i]

		err := os.Mkdir(dirPath, GeneratedDirMode)
		switch {
		case err == nil:
			if err := os.Chmod(dirPath, GeneratedDirMode); err != nil {
				return fmt.Errorf("setting mode on %q: %w", dirPath, err)
			}
		case errors.Is(err, os.ErrExist):
			info, statErr := os.Stat(dirPath)
			if statErr != nil {
				return fmt.Errorf("stating concurrently created %q: %w", dirPath, statErr)
			}
			if !info.IsDir() {
				return fmt.Errorf("path component %q is not a directory", dirPath)
			}
		default:
			return fmt.Errorf("creating directory %q: %w", dirPath, err)
		}

		parent := filepath.Dir(dirPath)
		if err := SyncDirectory(parent); err != nil {
			return fmt.Errorf("syncing directory entry for %q in %q: %w", dirPath, parent, err)
		}
	}

	return nil
}

func createTempFileForAtomicWrite(path string, mode os.FileMode) (*os.File, error) {
	if mode == security.SecretFileMode {
		return security.CreateSecretFile(path)
	}

	// G304 is suppressed: caller passes a validated absolute path rooted
	// in engine-managed state directories.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode) //nolint:gosec // G304: absolute path is validated by WriteFileAtomic
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(mode); err != nil {
		return nil, errors.Join(
			fmt.Errorf("chmod temp file %q: %w", path, err),
			closeAndRemoveTempFile(f, path),
		)
	}
	return f, nil
}

func writeAllAndSync(f *os.File, path string, data []byte) error {
	n, err := f.Write(data)
	if err != nil {
		return fmt.Errorf("state.WriteFileAtomic: writing temp file %q: %w", path, err)
	}
	if n != len(data) {
		return fmt.Errorf(
			"state.WriteFileAtomic: writing temp file %q: short write (%d/%d bytes)",
			path,
			n,
			len(data),
		)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("state.WriteFileAtomic: fsync temp file %q: %w", path, err)
	}
	return nil
}

func closeAndRemoveTempFile(f *os.File, path string) error {
	var errs []error
	if f != nil {
		if err := f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing temp file %q: %w", path, err))
		}
	}

	if err := removeTempFileIfPresent(path); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func removeTempFileIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing temp file %q: %w", path, err)
	}
	return nil
}

func validateFinalFileMode(path string, mode os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("state.WriteFileAtomic: stating final file %q: %w", path, err)
	}

	if mode == security.SecretFileMode {
		if err := security.ValidateSecretFileMode(info.Mode().Perm()); err != nil {
			return fmt.Errorf("state.WriteFileAtomic: validating secret mode for %q: %w", path, err)
		}
		return nil
	}

	got := info.Mode().Perm()
	want := mode.Perm()
	if got != want {
		return fmt.Errorf("state.WriteFileAtomic: validating mode for %q: got %#o, want %#o", path, got, want)
	}
	return nil
}
