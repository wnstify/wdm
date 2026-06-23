package core

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/wnstify/wdm/internal/release"
	"github.com/wnstify/wdm/pkg/types"
)

// This file hosts the CheckSelfUpdate engine method and the self-update
// helpers shared with the apply path in self_update_apply.go (PRD §14).
// CheckSelfUpdate is read-only, like [Engine.Status] / [Engine.
// CheckCatalogUpdate]: no runtime.lock, no Confirmer, no ProgressFn. It
// reports the running binary version, the latest verified release version,
// and whether a newer verified candidate exists — what the UI surfaces before
// any download or replace gates on it (the invariant: network actions
// explicit; the invariant: verify before apply).
// Network vs verification failures stay strictly distinct: a
// transport fault is [types.ErrCodeNetworkFailure] (exit 8) from the release
// client, a trust fault is [types.ErrCodeVerificationFailed] (exit 3) from
// the verifier. No telemetry is emitted on any path.
// Self-update version identity: the GitHub release tag (e.g. "v1.2.3") is the
// version. It is what the release pipeline stamps into the built binary
// (`-X main.version=<tag>`), so the running binary's [Engine.version] and the
// post-replace `wdm --version` smoke output are compared against it
// byte-exactly. A development build (version "dev") is never a release and
// never reports an available self-update.

// devVersion is the local-build version sentinel (cmd/wdm's Makefile default
// and [New]'s fallback). A binary stamped "dev" is not a published release,
// so CheckSelfUpdate reports no available update for it: a self-update over a
// dev build would replace a developer's working-tree binary with a release
// one, which is never the intent.
const devVersion = "dev"

// CheckSelfUpdate reports whether a newer verified wdm binary release exists
// (PRD §14). It is read-only: no runtime.lock, no Confirmer, no
// ProgressFn, like [Engine.Status] / [Engine.CheckCatalogUpdate].
// It reports the running binary version ([Engine.version]), resolves the
// latest release via the trusted release client (a transport fault is exit
// 8), downloads and verifies the latest candidate binary fail-closed into an
// ephemeral private staging directory that is removed on every path (a trust
// fault is exit 3, the invariant), and compares the verified candidate's
// release tag against the running version to decide whether an update is
// available. The live binary is NEVER touched — staging lands in an OS temp
// directory, not beside the install target.
// Verified is true only when the full checksum/signature/attestation chain
// passed; a release whose verification fails surfaces as an exit-3 error
// rather than an unverified "update available" — fail closed (PRD §22, §23).
func (e *Engine) CheckSelfUpdate(ctx context.Context, _ types.SelfUpdateQuery) (*types.SelfUpdateStatus, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.CheckSelfUpdate: %w", err)
	}

	client, err := e.releaseDeps.newReleaseClient()
	if err != nil {
		return nil, err
	}

	meta, err := client.LatestRelease(ctx)
	if err != nil {
		return nil, err
	}

	// Verify the candidate fail-closed: a trust failure
	// surfaces as exit 3 here rather than an unverified "update available".
	// Verification is coupled with staging in internal/release, so the check
	// stages into an EPHEMERAL private temp dir and removes it — the live
	// binary is never written. The staged candidate is discarded; only the
	// verified tag is kept.
	staged, cleanup, err := e.stageForVerification(ctx, client, meta)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	latest := strings.TrimSpace(staged.Tag)
	status := &types.SelfUpdateStatus{
		CurrentVersion:  e.version,
		LatestVersion:   latest,
		UpdateAvailable: selfUpdateAvailable(e.version, latest),
		Verified:        true,
		CheckedAt:       time.Now().UTC(),
	}
	if e.version == devVersion {
		status.Notes = append(status.Notes,
			"running a development build; self-update is not offered for dev binaries")
	}
	return status, nil
}

// stageForVerification downloads and verifies the latest candidate binary
// fail-closed into an ephemeral private staging directory, returning the
// verified [release.StagedCandidate] and a cleanup function that removes the
// staging directory. It is the read-path verify step: it proves the candidate
// passes checksum/signature/attestation without touching the
// live binary, because staging lands in an [os.MkdirTemp] directory (mode
// 0o700, never group/world-writable) rather than beside the install target.
// The cleanup function is always non-nil and safe to call (a no-op when no
// directory was created), so the caller can `defer cleanup`
// unconditionally. A transport fault is exit 8; a verification fault is exit
// 3 — both propagated unchanged.
func (e *Engine) stageForVerification(
	ctx context.Context,
	client *release.Client,
	meta *release.Metadata,
) (*release.StagedCandidate, func(), error) {
	stagingDir, err := os.MkdirTemp("", "wdm-selfupdate-check-*")
	if err != nil {
		return nil, func() {}, genericError(
			"could not create a staging directory for the self-update check",
			"",
			err,
		)
	}
	cleanup := func() { _ = os.RemoveAll(stagingDir) } //nolint:errcheck // best-effort removal of the ephemeral check staging dir

	staged, err := e.selfUpdateDeps.stageCandidate(ctx, client, meta, stagingDir)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return staged, cleanup, nil
}

// selfUpdateAvailable reports whether latest is a STRICTLY NEWER published
// release than the running current version. A development build ("dev") is
// never a release, so no update is offered for it. When both versions are valid
// semver the comparison is strict (latest > current), so a self-update can
// never downgrade or re-install the same version. When either side is not valid
// semver (an unstamped or dev-flavored build) it falls back to the prior
// "differs" behavior so those builds still see a published release as available.
func selfUpdateAvailable(current, latest string) bool {
	if current == devVersion {
		return false
	}
	if strings.TrimSpace(latest) == "" {
		return false
	}
	c, l := semver.Canonical(ensureV(current)), semver.Canonical(ensureV(latest))
	if semver.IsValid(c) && semver.IsValid(l) {
		return semver.Compare(l, c) > 0
	}
	return current != latest
}

// selfUpdateNotOlder reports whether candidate is acceptable to install over
// current at apply time: strictly newer when both are valid semver, otherwise
// (a dev/unstamped build on either side) it permits the apply, matching
// selfUpdateAvailable's fallback so non-release builds can still self-update.
// It is the apply-path re-assertion of the check-time downgrade guard.
func selfUpdateNotOlder(current, candidate string) bool {
	c, cand := semver.Canonical(ensureV(current)), semver.Canonical(ensureV(candidate))
	if semver.IsValid(c) && semver.IsValid(cand) {
		return semver.Compare(cand, c) > 0
	}
	return true
}

// ensureV prepends a leading "v" when missing so a bare version like "1.2.3"
// is accepted by golang.org/x/mod/semver, which requires the "v" prefix.
func ensureV(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}
