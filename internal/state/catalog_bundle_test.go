//go:build unix

package state_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
)

// tarMember is one entry to write into a test bundle. A trailing slash
// in Name or Dir:true marks a directory entry.
type tarMember struct {
	Name string
	Body string
	Dir  bool
	// Typeflag, when non-zero, overrides the default Reg/Dir choice so a
	// test can inject a hostile member type (symlink, hardlink, device).
	Typeflag byte
	Linkname string
}

// makeTarGz builds a gzip-compressed tar archive from members.
func makeTarGz(t *testing.T, members []tarMember) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		hdr := &tar.Header{Name: m.Name}
		switch {
		case m.Typeflag != 0:
			hdr.Typeflag = m.Typeflag
			hdr.Linkname = m.Linkname
			hdr.Mode = 0o644
		case m.Dir:
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Mode = 0o644
			hdr.Size = int64(len(m.Body))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if hdr.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(m.Body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// secureTempDir returns a 0o700 temp dir so secret-mode parent checks
// elsewhere stay satisfied on umask-0002 hosts (state fixture-hardening
// precedent).
func secureTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	return dir
}

func TestExtractTarGzToDir_HappyPathProducesContainedTree(t *testing.T) {
	t.Parallel()

	bundle := makeTarGz(t, []tarMember{
		{Name: "stable/", Dir: true},
		{Name: "stable/catalog.yaml", Body: "schema_version: 1\n"},
		{Name: "templates/", Dir: true},
		{Name: "templates/uptime-kuma/", Dir: true},
		{Name: "templates/uptime-kuma/docker-compose.yml.tmpl", Body: "services: {}\n"},
	})

	root := secureTempDir(t)
	dest := filepath.Join(root, "snapshot")

	require.NoError(t, state.ExtractTarGzToDir(context.Background(), bundle, dest))

	manifest, err := os.ReadFile(filepath.Join(dest, "stable", "catalog.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "schema_version: 1\n", string(manifest))

	tmpl, err := os.ReadFile(filepath.Join(dest, "templates", "uptime-kuma", "docker-compose.yml.tmpl"))
	require.NoError(t, err)
	assert.Equal(t, "services: {}\n", string(tmpl))

	// Restrictive modes: files 0o644, directories 0o755 (header modes ignored).
	fi, err := os.Stat(filepath.Join(dest, "stable", "catalog.yaml"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())

	di, err := os.Stat(filepath.Join(dest, "templates", "uptime-kuma"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), di.Mode().Perm())

	rootInfo, err := os.Stat(dest)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), rootInfo.Mode().Perm())
}

func TestExtractTarGzToDir_RejectsExistingDestination(t *testing.T) {
	t.Parallel()

	root := secureTempDir(t)
	dest := filepath.Join(root, "snapshot")
	require.NoError(t, os.Mkdir(dest, 0o755))

	bundle := makeTarGz(t, []tarMember{{Name: "stable/catalog.yaml", Body: "x"}})
	err := state.ExtractTarGzToDir(context.Background(), bundle, dest)
	require.Error(t, err)
	assert.ErrorIs(t, err, state.ErrBundleExtraction)
	assert.ErrorContains(t, err, "already exists")
}

func TestExtractTarGzToDir_HostileMemberNamesRejectedAndRolledBack(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		members []tarMember
	}{
		{
			name:    "parent traversal",
			members: []tarMember{{Name: "../escape.yaml", Body: "x"}},
		},
		{
			name:    "deep parent traversal",
			members: []tarMember{{Name: "stable/../../escape.yaml", Body: "x"}},
		},
		{
			name:    "absolute path",
			members: []tarMember{{Name: "/etc/passwd", Body: "x"}},
		},
		{
			name:    "symlink member",
			members: []tarMember{{Name: "stable/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
		},
		{
			name:    "hardlink member",
			members: []tarMember{{Name: "stable/hard", Typeflag: tar.TypeLink, Linkname: "stable/catalog.yaml"}},
		},
		{
			name:    "fifo member",
			members: []tarMember{{Name: "stable/pipe", Typeflag: tar.TypeFifo}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := secureTempDir(t)
			dest := filepath.Join(root, "snapshot")
			bundle := makeTarGz(t, tc.members)

			err := state.ExtractTarGzToDir(context.Background(), bundle, dest)
			require.Error(t, err)
			assert.ErrorIs(t, err, state.ErrBundleExtraction)

			// Rollback: the whole destination tree is removed.
			_, statErr := os.Lstat(dest)
			assert.True(t, errors.Is(statErr, os.ErrNotExist),
				"destination must be removed after a rejected member")
		})
	}
}

func TestExtractTarGzToDir_PathEscapeReachableThroughSentinel(t *testing.T) {
	t.Parallel()

	root := secureTempDir(t)
	dest := filepath.Join(root, "snapshot")
	bundle := makeTarGz(t, []tarMember{{Name: "../escape.yaml", Body: "x"}})

	err := state.ExtractTarGzToDir(context.Background(), bundle, dest)
	require.Error(t, err)
	assert.ErrorIs(t, err, state.ErrBundleExtraction)
	assert.ErrorIs(t, err, security.ErrPathEscape)
}

func TestExtractTarGzToDir_RejectsOverCapMembersAndSizes(t *testing.T) {
	t.Parallel()

	t.Run("too many members", func(t *testing.T) {
		t.Parallel()
		members := make([]tarMember, state.MaxBundleMembers+1)
		for i := range members {
			members[i] = tarMember{Name: "stable/f" + strconv.Itoa(i), Body: "x"}
		}
		root := secureTempDir(t)
		dest := filepath.Join(root, "snapshot")
		err := state.ExtractTarGzToDir(context.Background(), makeTarGz(t, members), dest)
		require.Error(t, err)
		assert.ErrorIs(t, err, state.ErrBundleExtraction)
		assert.ErrorContains(t, err, "member limit")
	})

	t.Run("single file over per-file cap", func(t *testing.T) {
		t.Parallel()
		big := bytes.Repeat([]byte("a"), int(state.MaxBundleFileBytes)+1)
		bundle := makeTarGz(t, []tarMember{{Name: "stable/big.yaml", Body: string(big)}})
		root := secureTempDir(t)
		dest := filepath.Join(root, "snapshot")
		err := state.ExtractTarGzToDir(context.Background(), bundle, dest)
		require.Error(t, err)
		assert.ErrorIs(t, err, state.ErrBundleExtraction)
		assert.ErrorContains(t, err, "size budget")
	})
}

func TestExtractTarGzToDir_RejectsEmptyAndRelativeInputs(t *testing.T) {
	t.Parallel()

	root := secureTempDir(t)
	dest := filepath.Join(root, "snapshot")

	t.Run("empty bundle", func(t *testing.T) {
		t.Parallel()
		err := state.ExtractTarGzToDir(context.Background(), nil, dest)
		require.Error(t, err)
		assert.ErrorIs(t, err, state.ErrBundleExtraction)
	})

	t.Run("relative destination", func(t *testing.T) {
		t.Parallel()
		bundle := makeTarGz(t, []tarMember{{Name: "stable/catalog.yaml", Body: "x"}})
		err := state.ExtractTarGzToDir(context.Background(), bundle, "relative/dest")
		require.Error(t, err)
		assert.ErrorIs(t, err, state.ErrBundleExtraction)
		assert.ErrorContains(t, err, "absolute")
	})

	t.Run("not gzip", func(t *testing.T) {
		t.Parallel()
		err := state.ExtractTarGzToDir(context.Background(), []byte("not a gzip stream"), filepath.Join(root, "ng"))
		require.Error(t, err)
		assert.ErrorIs(t, err, state.ErrBundleExtraction)
	})
}

func TestExtractTarGzToDir_FileThenChildDirFailsClosedAndRollsBack(t *testing.T) {
	t.Parallel()

	// A member creates "stable/x" as a regular file, then a later member
	// needs "stable/x/y" — the parent "stable/x" is a file, so the
	// directory creation must fail closed and roll back the whole tree.
	bundle := makeTarGz(t, []tarMember{
		{Name: "stable/x", Body: "i am a file"},
		{Name: "stable/x/y", Body: "child under a file"},
	})

	root := secureTempDir(t)
	dest := filepath.Join(root, "snapshot")
	err := state.ExtractTarGzToDir(context.Background(), bundle, dest)
	require.Error(t, err)
	assert.ErrorIs(t, err, state.ErrBundleExtraction)

	_, statErr := os.Lstat(dest)
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "destination removed on failure")
}

func TestExtractTarGzToDir_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	bundle := makeTarGz(t, []tarMember{{Name: "stable/catalog.yaml", Body: "x"}})
	root := secureTempDir(t)
	dest := filepath.Join(root, "snapshot")

	err := state.ExtractTarGzToDir(ctx, bundle, dest)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	_, statErr := os.Lstat(dest)
	assert.True(t, errors.Is(statErr, os.ErrNotExist))
}

func TestRemoveContainedTree_RemovesTreeAndTreatsAbsentAsSuccess(t *testing.T) {
	t.Parallel()

	root := secureTempDir(t)
	tree := filepath.Join(root, "tree")
	require.NoError(t, os.MkdirAll(filepath.Join(tree, "a"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "a", "f"), []byte("x"), 0o644))

	require.NoError(t, state.RemoveContainedTree(tree))
	_, statErr := os.Lstat(tree)
	assert.True(t, errors.Is(statErr, os.ErrNotExist))

	// Removing an already-absent path is success.
	require.NoError(t, state.RemoveContainedTree(tree))
}

func TestRemoveContainedTree_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	err := state.RemoveContainedTree("relative/path")
	require.Error(t, err)
	assert.ErrorContains(t, err, "absolute")
}

func TestCopyTree_HappyPathCopiesAndNormalizesModes(t *testing.T) {
	t.Parallel()

	root := secureTempDir(t)
	src := filepath.Join(root, "src")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "top.txt"), []byte("top"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a", "b", "deep.txt"), []byte("deep"), 0o600))

	dest := filepath.Join(root, "dest")
	require.NoError(t, state.CopyTree(src, dest))

	top, err := os.ReadFile(filepath.Join(dest, "top.txt"))
	require.NoError(t, err)
	assert.Equal(t, "top", string(top))

	deep, err := os.ReadFile(filepath.Join(dest, "a", "b", "deep.txt"))
	require.NoError(t, err)
	assert.Equal(t, "deep", string(deep))

	fi, err := os.Stat(filepath.Join(dest, "top.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())
}

func TestCopyTree_RejectsExistingDestinationAndIrregularSource(t *testing.T) {
	t.Parallel()

	root := secureTempDir(t)
	src := filepath.Join(root, "src")
	require.NoError(t, os.Mkdir(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644))

	t.Run("existing destination", func(t *testing.T) {
		t.Parallel()
		dest := filepath.Join(root, "existing")
		require.NoError(t, os.Mkdir(dest, 0o755))
		err := state.CopyTree(src, dest)
		require.Error(t, err)
		assert.ErrorIs(t, err, state.ErrBundleExtraction)
		assert.ErrorContains(t, err, "already exists")
	})

	t.Run("symlink entry in source fails closed and rolls back", func(t *testing.T) {
		t.Parallel()
		ssrc := filepath.Join(root, "ssrc")
		require.NoError(t, os.Mkdir(ssrc, 0o755))
		require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(ssrc, "link")))
		dest := filepath.Join(root, "sdest")
		err := state.CopyTree(ssrc, dest)
		require.Error(t, err)
		assert.ErrorIs(t, err, state.ErrBundleExtraction)
		_, statErr := os.Lstat(dest)
		assert.True(t, errors.Is(statErr, os.ErrNotExist), "destination removed on failure")
	})
}
