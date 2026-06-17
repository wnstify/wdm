//go:build unix

package state

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/wnstify/wdm/internal/security"
)

// Bundle-extraction guard rails. These bound the work a single
// verified-catalog bundle can demand so a malformed or hostile
// archive cannot exhaust memory or disk (zip-bomb / member-flood
// defense in depth). The stable catalog bundle is tens of KiB of
// YAML and templates, so the caps never bind on honest input.
const (
	// MaxBundleMembers caps the number of tar entries a bundle may
	// contain before extraction fails closed.
	MaxBundleMembers = 4096

	// MaxBundleFileBytes caps the uncompressed size of any single
	// extracted file.
	MaxBundleFileBytes int64 = 16 << 20 // 16 MiB

	// MaxBundleTotalBytes caps the total uncompressed size across all
	// extracted files in one bundle.
	MaxBundleTotalBytes int64 = 64 << 20 // 64 MiB

	// catalogBundleFileMode is the mode every extracted regular file is
	// written with. Catalog manifests and templates are non-secret
	// world-readable config, so 0o644 — the tar header's own mode bits
	// are deliberately ignored.
	catalogBundleFileMode os.FileMode = 0o644
)

// ErrBundleExtraction is returned (wrapped) by [ExtractTarGzToDir]
// and [CopyTree] for every failure: malformed archive, a hostile
// member name, an over-cap member, or an underlying filesystem
// error. The wrapping context names the offending member or path.
// Detect the class with [errors.Is].
// Path-containment rejections additionally wrap
// [security.ErrPathEscape] / [security.ErrUnsafePath], which stay
// reachable through this sentinel via [errors.Is].
var ErrBundleExtraction = errors.New("state: catalog bundle extraction failed")

// ExtractTarGzToDir extracts a gzip-compressed tar bundle into a
// freshly created destination directory, treating every archive
// member name as hostile.
// This is the byte-level SINK for a verified catalog bundle. Trust
// verification that the bundle bytes are authentic happens upstream in
// internal/release; this function never trusts the
// archive's structure. It is catalog-shape agnostic — the layout
// policy (channel, version directory, active-manifest placement) lives
// in internal/catalog, which drives this primitive.
// destDir MUST be absolute and MUST NOT already exist: it is created
// at [GeneratedDirMode] and, on ANY failure, removed in full so a
// half-extracted tree is never left behind (rollback). A pre-existing
// destDir is rejected rather than merged, so the caller's prior state
// is never partially overwritten.
// Member discipline (defense in depth, PRD §12/§13):
//   - Only regular files and directories are extracted. Symlinks,
//     hardlinks, character/block devices, and FIFOs are rejected —
//     a symlink member could otherwise redirect a later write outside
//     destDir.
//   - Each member name is joined to destDir through
//     [security.SafeJoin], which rejects absolute paths, parent
//     traversal ("../"), and volume references before any write.
//   - Extracted regular files are written at [catalogBundleFileMode]
//     (0o644) and directories at [GeneratedDirMode] (0o755); the tar
//     header's own mode bits are ignored.
//   - Member count and per-file / total uncompressed size are bounded
//     by [MaxBundleMembers], [MaxBundleFileBytes], and
//     [MaxBundleTotalBytes].
//
// The context is honored between members so a long extraction is
// cancellable.
func ExtractTarGzToDir(ctx context.Context, bundle []byte, destDir string) (err error) {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrBundleExtraction, err)
	}
	if destDir == "" || !filepath.IsAbs(destDir) {
		return fmt.Errorf("%w: destination %q must be absolute", ErrBundleExtraction, destDir)
	}
	if len(bundle) == 0 {
		return fmt.Errorf("%w: bundle is empty", ErrBundleExtraction)
	}

	if _, statErr := os.Lstat(destDir); statErr == nil {
		return fmt.Errorf("%w: destination %q already exists", ErrBundleExtraction, destDir)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("%w: stating destination %q: %w", ErrBundleExtraction, destDir, statErr)
	}

	if mkErr := os.Mkdir(destDir, GeneratedDirMode); mkErr != nil {
		return fmt.Errorf("%w: creating destination %q: %w", ErrBundleExtraction, destDir, mkErr)
	}
	if chmodErr := os.Chmod(destDir, GeneratedDirMode); chmodErr != nil {
		return errors.Join(
			fmt.Errorf("%w: setting mode on destination %q: %w", ErrBundleExtraction, destDir, chmodErr),
			removeTreeIfPresent(destDir),
		)
	}
	// Roll back the whole destination tree on any failure after creation.
	defer func() {
		if err != nil {
			err = errors.Join(err, removeTreeIfPresent(destDir))
		}
	}()

	if syncErr := SyncDirectory(filepath.Dir(destDir)); syncErr != nil {
		return fmt.Errorf("%w: syncing parent of %q: %w", ErrBundleExtraction, destDir, syncErr)
	}

	if err := extractMembers(ctx, bundle, destDir); err != nil {
		return err
	}
	if syncErr := SyncDirectory(destDir); syncErr != nil {
		return fmt.Errorf("%w: syncing destination %q: %w", ErrBundleExtraction, destDir, syncErr)
	}
	return nil
}

func extractMembers(ctx context.Context, bundle []byte, destDir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return fmt.Errorf("%w: opening gzip stream: %w", ErrBundleExtraction, err)
	}
	defer func() { _ = gz.Close() }() //nolint:errcheck // best-effort reader teardown; the read path's own errors are authoritative

	tr := tar.NewReader(gz)
	var (
		members   int
		totalSize int64
	)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrBundleExtraction, err)
		}

		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: reading tar entry: %w", ErrBundleExtraction, err)
		}

		members++
		if members > MaxBundleMembers {
			return fmt.Errorf("%w: bundle exceeds %d member limit", ErrBundleExtraction, MaxBundleMembers)
		}

		cleanName := path.Clean(hdr.Name)
		if cleanName == "." || cleanName == "" {
			// The archive root entry itself; nothing to create.
			continue
		}
		target, err := security.SafeJoin(destDir, filepath.FromSlash(cleanName))
		if err != nil {
			return fmt.Errorf("%w: rejecting member %q: %w", ErrBundleExtraction, hdr.Name, err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkdirAllBundle(target); err != nil {
				return err
			}
		case tar.TypeReg:
			written, err := extractRegularFile(tr, target, hdr.Name, totalSize)
			if err != nil {
				return err
			}
			totalSize += written
		default:
			return fmt.Errorf(
				"%w: member %q has unsupported type %q (only regular files and directories are allowed)",
				ErrBundleExtraction, hdr.Name, string(hdr.Typeflag),
			)
		}
	}
	return nil
}

func extractRegularFile(tr io.Reader, target, memberName string, totalSoFar int64) (int64, error) {
	if err := mkdirAllBundle(filepath.Dir(target)); err != nil {
		return 0, err
	}

	// Bound this member to the smaller of the per-file cap and the
	// remaining total budget, plus one byte so an over-cap member is
	// detected rather than silently truncated.
	remaining := MaxBundleTotalBytes - totalSoFar
	if remaining < 0 {
		remaining = 0
	}
	limit := MaxBundleFileBytes
	if remaining < limit {
		limit = remaining
	}

	data, err := io.ReadAll(io.LimitReader(tr, limit+1))
	if err != nil {
		return 0, fmt.Errorf("%w: reading member %q: %w", ErrBundleExtraction, memberName, err)
	}
	if int64(len(data)) > limit {
		return 0, fmt.Errorf(
			"%w: member %q exceeds size budget (per-file %d / total %d bytes)",
			ErrBundleExtraction, memberName, MaxBundleFileBytes, MaxBundleTotalBytes,
		)
	}

	if err := WriteFileAtomic(target, data, catalogBundleFileMode); err != nil {
		return 0, fmt.Errorf("%w: writing member %q: %w", ErrBundleExtraction, memberName, err)
	}
	return int64(len(data)), nil
}

// CopyTree recursively copies the regular-file/directory tree rooted
// at srcDir into a freshly created destDir, normalizing modes to
// [catalogBundleFileMode] (files) and [GeneratedDirMode] (dirs).
// It is the byte-level primitive internal/catalog uses to materialize
// the active templates tree from an immutable verified snapshot
// without consuming the snapshot. Both paths MUST be absolute; destDir
// MUST NOT already exist and is removed in full on any failure
// (rollback). srcDir MUST be a directory whose entries are all regular
// files or directories — any symlink, device, or other irregular entry
// fails closed, mirroring [ExtractTarGzToDir]'s member discipline so a
// snapshot tampered with after extraction still cannot redirect a write.
func CopyTree(srcDir, destDir string) (err error) {
	if srcDir == "" || !filepath.IsAbs(srcDir) {
		return fmt.Errorf("%w: source %q must be absolute", ErrBundleExtraction, srcDir)
	}
	if destDir == "" || !filepath.IsAbs(destDir) {
		return fmt.Errorf("%w: destination %q must be absolute", ErrBundleExtraction, destDir)
	}

	srcInfo, err := os.Lstat(srcDir)
	if err != nil {
		return fmt.Errorf("%w: stating source %q: %w", ErrBundleExtraction, srcDir, err)
	}
	if !srcInfo.IsDir() {
		return fmt.Errorf("%w: source %q is not a directory", ErrBundleExtraction, srcDir)
	}
	if _, statErr := os.Lstat(destDir); statErr == nil {
		return fmt.Errorf("%w: destination %q already exists", ErrBundleExtraction, destDir)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("%w: stating destination %q: %w", ErrBundleExtraction, destDir, statErr)
	}

	if mkErr := os.Mkdir(destDir, GeneratedDirMode); mkErr != nil {
		return fmt.Errorf("%w: creating destination %q: %w", ErrBundleExtraction, destDir, mkErr)
	}
	if chmodErr := os.Chmod(destDir, GeneratedDirMode); chmodErr != nil {
		return errors.Join(
			fmt.Errorf("%w: setting mode on destination %q: %w", ErrBundleExtraction, destDir, chmodErr),
			removeTreeIfPresent(destDir),
		)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeTreeIfPresent(destDir))
		}
	}()

	if err := copyTreeContents(srcDir, destDir); err != nil {
		return err
	}
	if syncErr := SyncDirectory(destDir); syncErr != nil {
		return fmt.Errorf("%w: syncing destination %q: %w", ErrBundleExtraction, destDir, syncErr)
	}
	return nil
}

func copyTreeContents(srcDir, destDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("%w: reading source %q: %w", ErrBundleExtraction, srcDir, err)
	}

	for _, entry := range entries {
		srcChild := filepath.Join(srcDir, entry.Name())
		destChild, joinErr := security.SafeJoin(destDir, entry.Name())
		if joinErr != nil {
			return fmt.Errorf("%w: rejecting entry %q: %w", ErrBundleExtraction, entry.Name(), joinErr)
		}

		switch {
		case entry.IsDir():
			if err := mkdirAllBundle(destChild); err != nil {
				return err
			}
			if err := copyTreeContents(srcChild, destChild); err != nil {
				return err
			}
		case entry.Type().IsRegular():
			// G304: srcChild is built from os.ReadDir entries under an
			// absolute caller-controlled directory; the SafeJoin above
			// guards the destination.
			data, readErr := os.ReadFile(srcChild) //nolint:gosec // G304: srcChild is under the absolute caller-controlled srcDir
			if readErr != nil {
				return fmt.Errorf("%w: reading %q: %w", ErrBundleExtraction, srcChild, readErr)
			}
			if writeErr := WriteFileAtomic(destChild, data, catalogBundleFileMode); writeErr != nil {
				return fmt.Errorf("%w: writing %q: %w", ErrBundleExtraction, destChild, writeErr)
			}
		default:
			return fmt.Errorf(
				"%w: entry %q is not a regular file or directory",
				ErrBundleExtraction, srcChild,
			)
		}
	}
	return nil
}

func mkdirAllBundle(dir string) error {
	if err := os.MkdirAll(dir, GeneratedDirMode); err != nil {
		return fmt.Errorf("%w: creating directory %q: %w", ErrBundleExtraction, dir, err)
	}
	if err := os.Chmod(dir, GeneratedDirMode); err != nil {
		return fmt.Errorf("%w: setting mode on directory %q: %w", ErrBundleExtraction, dir, err)
	}
	return nil
}

// RemoveContainedTree removes treePath and everything under it via
// os.RemoveAll, treating an already-absent path as success. It is the
// single sanctioned destructive-removal site for the catalog-storage
// rollback path: internal/state owns byte-level filesystem mechanics,
// so internal/catalog's storage writer rolls partial work back through
// this helper rather than calling os.RemoveAll itself, keeping all
// catalog-storage RemoveAll on the closed allowlist in one file.
// treePath MUST be an absolute path the caller created or owns; the
// helper performs no containment of its own beyond the absolute-path
// guard, deferring path-safety to the caller (the storage writer joins
// every path through security.SafeJoin upstream).
func RemoveContainedTree(treePath string) error {
	if treePath == "" || !filepath.IsAbs(treePath) {
		return fmt.Errorf("state.RemoveContainedTree: path must be absolute, got %q", treePath)
	}
	return removeTreeIfPresent(treePath)
}

// removeTreeIfPresent removes treePath and everything under it,
// treating an already-absent path as success.
func removeTreeIfPresent(treePath string) error {
	if err := os.RemoveAll(treePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %q: %w", treePath, err)
	}
	return nil
}
