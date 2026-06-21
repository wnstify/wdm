package logging_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/logging"
	"github.com/wnstify/wdm/internal/security"
)

// writeArchive seeds dir with an archive file whose mod time is set to age
// ago so retention pruning has aged inputs to act on.
func writeArchive(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("seed\n"), 0o600))
	mod := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, mod, mod))
	return path
}

func listArchives(t *testing.T, dir string) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, "wdm-*.log"))
	require.NoError(t, err)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, filepath.Base(m))
	}
	sort.Strings(names)
	return names
}

func TestOpenLogFile_ArchivesPriorLatestAndOpensFresh(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "logs")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, logging.LatestLogName), []byte("prior session\n"), 0o600))

	f, err := logging.OpenLogFile(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	info, err := os.Stat(filepath.Join(dir, logging.LatestLogName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "fresh latest.log must be owner-only")
	assert.Zero(t, info.Size(), "fresh latest.log must start empty")

	archives := listArchives(t, dir)
	require.Len(t, archives, 1, "prior latest.log must be archived")

	body, err := os.ReadFile(filepath.Join(dir, archives[0]))
	require.NoError(t, err)
	assert.Equal(t, "prior session\n", string(body), "archive must hold the prior session content")

	dirInfo, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm(), "logs dir must be owner-only")
}

func TestOpenLogFile_RetentionIntersection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		seed func(t *testing.T, dir string) (wantKept, wantPruned []string)
	}{
		{
			name: "prunes archives older than max age",
			seed: func(t *testing.T, dir string) ([]string, []string) {
				fresh := filepath.Base(writeArchive(t, dir, "wdm-2026-06-01-120000.log", 24*time.Hour))
				aged := filepath.Base(writeArchive(t, dir, "wdm-2026-01-01-120000.log", logging.RetentionMaxAge+48*time.Hour))
				return []string{fresh}, []string{aged}
			},
		},
		{
			name: "prunes archives beyond the file cap even when young",
			seed: func(t *testing.T, dir string) ([]string, []string) {
				var kept, pruned []string
				// keepCount = RetentionMaxFiles-1 newest archives survive;
				// older-by-mtime ones past that fall out. Stagger mod times
				// by minute so ordering is deterministic.
				total := logging.RetentionMaxFiles + 5
				for i := range total {
					age := time.Duration(i) * time.Minute
					name := filepath.Base(writeArchive(t, dir, archiveName(i), age))
					if i < logging.RetentionMaxFiles-1 {
						kept = append(kept, name)
					} else {
						pruned = append(pruned, name)
					}
				}
				return kept, pruned
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := filepath.Join(t.TempDir(), "logs")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			wantKept, wantPruned := tt.seed(t, dir)

			f, err := logging.OpenLogFile(dir)
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })

			survivors := listArchives(t, dir)
			for _, name := range wantKept {
				assert.Contains(t, survivors, name, "archive within retention must survive")
			}
			for _, name := range wantPruned {
				assert.NotContains(t, survivors, name, "archive outside retention must be pruned")
			}
			assert.FileExists(t, filepath.Join(dir, logging.LatestLogName), "latest.log must always survive")
			assert.LessOrEqual(t, len(survivors)+1, logging.RetentionMaxFiles, "archives plus latest.log must stay within the file cap")
		})
	}
}

// archiveName returns a unique archive file name for index i, keeping the
// PRD §24 wdm-*.log shape while staying distinct per seed.
func archiveName(i int) string {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	return "wdm-" + base.Add(time.Duration(i)*time.Hour).Format("2006-01-02-150405") + ".log"
}

func TestOpenLogFile_RedactionHoldsForFileSink(t *testing.T) {
	t.Parallel()

	const (
		password = "hunter2-super-secret"
		token    = "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		secret   = "GENERATEDSECRETVALUE1234567890"
		privKey  = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"
		envBody  = "POSTGRES_PASSWORD=hunter2-super-secret\nAPI_TOKEN=ghp_aaaa"
	)

	dir := filepath.Join(t.TempDir(), "logs")
	f, err := logging.OpenLogFile(dir)
	require.NoError(t, err)

	// Model the production wiring: generated secrets (incl. minted private
	// keys) are registered with the redactor so its literal scrub catches
	// them in any string an attr carries, on top of the structural patterns.
	// The logger is built the same way buildDefaultLogger wires the engine's
	// sink so the file-sink redaction stays covered through the prod chain.
	redactor := security.NewActiveRedactor([]string{password, token, secret, privKey, envBody})
	logger := newJSONLogger(f, slog.LevelInfo, false, redactor)

	logger.Info("install secrets minted",
		slog.String("password", password),
		slog.String("token", token),
		slog.String("secret", secret),
		slog.String("private_key", privKey),
		slog.String("env", envBody),
	)
	require.NoError(t, f.Close())

	raw, err := os.ReadFile(filepath.Join(dir, logging.LatestLogName))
	require.NoError(t, err)
	content := string(raw)

	for _, leak := range []string{password, token, secret, privKey, envBody} {
		assert.NotContains(t, content, leak, "secret value must never reach the file sink")
	}
	assert.Contains(t, content, security.RedactedPlaceholder, "redacted values must be replaced with the placeholder")
}

func TestOpenLogFile_FailsSoftIsReportedToCaller(t *testing.T) {
	t.Parallel()

	// A regular file standing where the logs dir should be makes MkdirAll
	// fail; the engine treats the returned error as a fallback trigger.
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("not a dir\n"), 0o600))

	f, err := logging.OpenLogFile(filepath.Join(blocker, "logs"))
	require.Error(t, err, "an un-creatable log dir must surface an error, not panic")
	assert.Nil(t, f)
}

func TestOpenLogFile_RejectsRelativeDir(t *testing.T) {
	t.Parallel()

	f, err := logging.OpenLogFile("relative/logs")
	require.Error(t, err)
	assert.Nil(t, f)
}

// guardSecretShape keeps the redaction test honest: if the placeholder ever
// becomes a substring of a seeded secret the NotContains assertions would
// pass vacuously.
func TestRedactionPlaceholderNotSubstringOfSecrets(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"hunter2-super-secret", "ghp_aaaa", "GENERATEDSECRETVALUE1234567890"} {
		assert.False(t, strings.Contains(s, security.RedactedPlaceholder))
	}
}

// TestOpenLogFile_StatFaultDoesNotHang proves the archive collision probe
// fails soft instead of spinning forever when Lstat returns a non-IsNotExist
// error. Removing the dir's search bit makes Lstat of an entry inside return
// EACCES; the probe must break on that fault and surface a Rename error
// promptly rather than looping (Fix 1, PRD §24).
func TestOpenLogFile_StatFaultDoesNotHang(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("EACCES via search-bit removal is not forceable on windows")
	}

	dir := filepath.Join(t.TempDir(), "logs")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, logging.LatestLogName), []byte("prior\n"), 0o600))

	// Drop the search/exec bit so Lstat of any entry inside the dir faults
	// with EACCES (not IsNotExist), exercising the non-IsNotExist break.
	require.NoError(t, os.Chmod(dir, 0o600))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	done := make(chan error, 1)
	go func() {
		f, err := logging.OpenLogFile(dir)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "stat fault must surface an error, not be ignored")
	case <-time.After(2 * time.Second):
		t.Fatal("OpenLogFile hung on the archive collision probe")
	}
}

// TestOpenLogFile_SameSecondCollisionBumpsCounter covers the archive
// collision-counter branch (Fix 1): when the base wdm-<ts>.log name for the
// current second already exists, archiveLatest must not clobber it but suffix
// a -N counter. OpenLogFile uses time.Now() internally, so the base archive
// names for the current second and the next second are both pre-seeded; the
// rename therefore collides regardless of which second it lands in and must
// produce a -1 counter-suffixed archive.
func TestOpenLogFile_SameSecondCollisionBumpsCounter(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "logs")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, logging.LatestLogName), []byte("prior\n"), 0o600))

	// Occupy the base archive name for the current and next second so the
	// rename target is taken whichever boundary time.Now() falls on.
	const layout = "2006-01-02-150405"
	now := time.Now()
	for _, ts := range []string{now.Format(layout), now.Add(time.Second).Format(layout)} {
		base := filepath.Join(dir, "wdm-"+ts+".log")
		require.NoError(t, os.WriteFile(base, []byte("existing archive\n"), 0o600))
	}

	f, err := logging.OpenLogFile(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	// The prior latest.log must have landed in a -1 counter-suffixed archive,
	// proving the base name was not clobbered.
	counter, err := filepath.Glob(filepath.Join(dir, "wdm-*-1.log"))
	require.NoError(t, err)
	require.Len(t, counter, 1, "collision must route the prior latest.log to a -1 archive")

	body, err := os.ReadFile(counter[0])
	require.NoError(t, err)
	assert.Equal(t, "prior\n", string(body), "counter archive must hold the prior session, not clobber an existing archive")
}

// TestOpenLogFile_PruneUsesLstatNotTarget covers Fix 2: prune must decide
// retention from each entry's own metadata via Lstat, never by following a
// symlink to its target. The wdm-*.log archive is a symlink whose own mod time
// is fresh but whose target is aged well past RetentionMaxAge. The old os.Stat
// would follow the link and prune it on the target's age; os.Lstat reads the
// link's own fresh mod time, so the link must survive.
func TestOpenLogFile_PruneUsesLstatNotTarget(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on windows")
	}

	dir := filepath.Join(t.TempDir(), "logs")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	// Aged target outside retention; the symlink's own mod time stays fresh.
	target := filepath.Join(dir, "target.log")
	require.NoError(t, os.WriteFile(target, []byte("aged\n"), 0o600))
	aged := time.Now().Add(-(logging.RetentionMaxAge + 48*time.Hour))
	require.NoError(t, os.Chtimes(target, aged, aged))

	link := filepath.Join(dir, "wdm-2020-01-01-000000.log")
	require.NoError(t, os.Symlink(target, link))

	f, err := logging.OpenLogFile(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	// Lstat saw the link's fresh mod time, not the aged target, so the
	// symlink archive survives pruning.
	_, statErr := os.Lstat(link)
	require.NoError(t, statErr, "prune must keep the symlink by its own (fresh) mod time, not follow it to the aged target")
}
