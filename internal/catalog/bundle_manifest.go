//go:build unix

package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/wnstify/wdm/internal/state"
)

// ReadBundleManifest reads and validates ONLY the channel manifest
// (stable/catalog.yaml) from an in-memory gzip-tar catalog bundle,
// returning the parsed [*Catalog] without writing to disk
// The catalog-update apply path needs the candidate bundle's version
// (Catalog.GeneratedAt) BEFORE it writes anything, so it can refuse a
// signed rollback without
// the cost of a full extraction. This reader returns the manifest and
// nothing else; the full atomic write is [StoreVerifiedCatalog]'s job.
// The bundle bytes are already trust-verified upstream (internal/release),
// but this reader still treats the archive structure as untrusted: it
// streams the gzip-tar, reads only the fixed [bundleManifestRelPath]
// member, caps the decompressed manifest at [state.MaxBundleFileBytes]
// (zip-bomb defense in depth), and skips any non-regular member. A
// missing manifest, an over-cap manifest, a malformed archive, or a
// manifest that fails schema validation all fail closed with a wrapped
// error ([ErrCatalogStorage] for structural faults, [ErrCatalogInvalid]
// for a manifest the loader rejects), reachable via [errors.Is].
func ReadBundleManifest(ctx context.Context, bundle []byte) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCatalogStorage, err)
	}
	if len(bundle) == 0 {
		return nil, fmt.Errorf("%w: bundle is empty", ErrCatalogStorage)
	}

	manifestBytes, err := readBundleMember(bundle, bundleManifestRelPath)
	if err != nil {
		return nil, err
	}

	cat, err := LoadCatalogBytes(ctx, manifestBytes)
	if err != nil {
		// LoadCatalogBytes wraps ErrCatalogInvalid; surface it unchanged so
		// errors.Is reaches the right sentinel for the caller's mapping.
		return nil, err
	}
	return cat, nil
}

// readBundleMember streams the gzip-tar bundle and returns the bytes of
// the single regular-file member whose cleaned name equals want, capped
// at [state.MaxBundleFileBytes]. It mirrors the member discipline of
// internal/state's bundle extractor (only regular files; over-cap members
// refused, not truncated) so the manifest peek and the later full
// extraction agree on what the bundle may contain.
func readBundleMember(bundle []byte, want string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return nil, fmt.Errorf("%w: opening gzip stream: %w", ErrCatalogStorage, err)
	}
	defer func() { _ = gz.Close() }() //nolint:errcheck // best-effort reader teardown; the read path's own errors are authoritative

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: reading tar entry: %w", ErrCatalogStorage, err)
		}
		if path.Clean(hdr.Name) != want || hdr.Typeflag != tar.TypeReg {
			continue
		}

		// Read one byte past the cap so an over-cap manifest is detected,
		// not silently truncated.
		data, err := io.ReadAll(io.LimitReader(tr, state.MaxBundleFileBytes+1))
		if err != nil {
			return nil, fmt.Errorf("%w: reading bundle manifest: %w", ErrCatalogStorage, err)
		}
		if int64(len(data)) > state.MaxBundleFileBytes {
			return nil, fmt.Errorf("%w: bundle manifest exceeds %d bytes", ErrCatalogStorage, state.MaxBundleFileBytes)
		}
		return data, nil
	}

	return nil, fmt.Errorf("%w: bundle is missing %s", ErrCatalogStorage, want)
}
