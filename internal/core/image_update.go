package core

import (
	"context"
	"fmt"
	"time"

	"github.com/wnstify/wdm/internal/registry"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// This file hosts the CheckImageUpdates engine method.
// CheckImageUpdates is Go-native and read-only (no runtime.lock, no
// Confirmer, no ProgressFn), like Status: it contacts the registry through
// Go HTTP — NEVER `docker manifest inspect` or any Docker —
// and feeds the existing app-update planning surface. There is no apply
// counterpart: app image updates apply only through Update.
// Network vs verification: a registry transport / auth /
// rate-limit fault is [types.ErrCodeNetworkFailure] (exit 8), surfaced from
// the registry client unchanged. The client performs no trust verification,
// so this path never produces an exit-3 verification error. The check is
// anonymous and public-only — no credentials — and mutates no
// stack file on any path.

// CheckImageUpdates reports registry-derived tag/digest candidates for a
// managed app's service images (PRD §14, §20). It is
// read-only: no runtime.lock, no Confirmer, no ProgressFn, like
// [Engine.Status].
// Managed-only ordering (PRD §10): the stack must resolve to a directory
// whose .wdm.lock parses and names query.AppID before any registry call.
// Unmanaged directories and uninstalled apps refuse with
// [types.ErrCodeUsageValidation]; a stack mid-operation refuses with
// [types.ErrCodeRuntimeLockHeld]; a corrupt manifest surfaces wrapped
// [types.ErrStaleState]. The non-blocking shared-flock read shared with
// Status drives this, so the check never stalls behind a writer and never
// acquires runtime.lock.
// For each manifest image pin carrying a tag, the check resolves the
// catalog-pinned tag to its canonical registry digest and reports the
// current pinned digest, the registry digest behind the same tag, and
// whether they differ (PRD §20 tag+digest visibility). It NEVER picks a
// different tag than the manifest pins — the catalog stays the update source
// it only surfaces the digest behind the pinned tag.
// A digest-only pin (no tag) has no tag to resolve, so its candidate carries
// the current digest but empty LatestTag/LatestDigest and
// UpdateAvailable=false — the "registry info unavailable for this service"
// state, representable in the frozen [types.ImageUpdateReport] /
// [types.ImageUpdateCandidate] shape without a marker field. A genuine
// registry transport failure on the EXPLICIT check is NOT degraded — it
// surfaces exit 8 per the invariant; opportunistic degradation lives only in
// the Update-planning fold-in ([Engine.resolveRegistryDigests]).
func (e *Engine) CheckImageUpdates(ctx context.Context, query types.ImageUpdateQuery) (*types.ImageUpdateReport, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.CheckImageUpdates: %w", err)
	}
	if query.AppID == "" {
		return nil, usageValidationError(
			"app id is required",
			"pass the app id of an installed stack",
			nil,
		)
	}

	_, lock, err := e.resolveManagedStack(ctx, query.AppID)
	if err != nil {
		return nil, err
	}

	client := e.newRegistryClient()
	if client == nil {
		// The explicit check is non-opportunistic: unlike the
		// Update-planning fold-in's nil tolerance ([Engine.resolveRegistryDigests]),
		// a nil client here cannot silently degrade — calling through it would
		// panic, violating the no-panic-in-internal/core invariant — so it
		// fails closed with a typed error.
		return nil, genericError("registry client is unavailable", "", nil)
	}

	candidates, err := resolveImageUpdateCandidates(ctx, client, lock.ImagePins)
	if err != nil {
		return nil, err
	}

	return &types.ImageUpdateReport{
		AppID:      query.AppID,
		Candidates: candidates,
		CheckedAt:  time.Now().UTC(),
	}, nil
}

// resolveImageUpdateCandidates resolves each manifest image pin's pinned tag
// to its registry digest and projects the per-service findings. A pin with
// an empty service is skipped; a digest-only pin (no tag) is reported as
// registry-info-unavailable (empty latest fields, UpdateAvailable=false). A
// registry transport / usage fault on any pin propagates UNCHANGED so the
// explicit check fails closed with the registry client's typed code (network
// → exit 8, malformed ref → exit 2; the invariant). The returned slice is in
// manifest-pin order with empty-service pins removed.
func resolveImageUpdateCandidates(
	ctx context.Context,
	client RegistryResolver,
	pins []state.ImagePin,
) ([]types.ImageUpdateCandidate, error) {
	var candidates []types.ImageUpdateCandidate
	for _, pin := range pins {
		if pin.Service == "" {
			continue
		}
		candidate := types.ImageUpdateCandidate{
			Service:       pin.Service,
			Image:         pin.Image,
			CurrentTag:    pin.Tag,
			CurrentDigest: pin.Digest,
		}
		if pin.Tag == "" {
			// Digest-only pin: no tag to resolve, so report the current digest
			// only — the per-service registry-info-unavailable state.
			candidates = append(candidates, candidate)
			continue
		}

		manifest, err := client.ResolveDigest(ctx, updateImageRef(pin.Image, pin.Tag))
		if err != nil {
			// Fail closed with the registry client's typed code.
			// Opportunistic degradation is the Update-planning fold-in's job.
			return nil, err
		}

		candidate.LatestTag = pin.Tag
		candidate.LatestDigest = manifest.Digest
		candidate.UpdateAvailable = imageUpdateAvailable(pin.Digest, manifest.Digest)
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

// imageUpdateAvailable reports whether the registry digest behind the pinned
// tag differs from the digest the stack currently records for that same tag.
// An empty current digest (never recorded — opportunistic capture absence
// per the confirmation rules) is NOT reported as an update: with no recorded baseline to
// compare against, a missing baseline cannot masquerade as a pending update.
// An empty registry digest likewise yields no update.
func imageUpdateAvailable(currentDigest, registryDigest string) bool {
	if currentDigest == "" || registryDigest == "" {
		return false
	}
	return currentDigest != registryDigest
}

// resolveRegistryDigests is the opportunistic registry-digest fold-in for
// [Engine.Update] planning. It resolves the
// registry digest behind each changed service's CANDIDATE (catalog-pinned)
// tag so the planning stream can disclose the digest the catalog tag
// resolves to (PRD §20 tag+digest visibility), WITHOUT changing what the
// update applies — the catalog remains the update source and no
// registry-chosen tag is ever substituted.
// It is opportunistic and never-fail: a registry transport failure, a
// malformed candidate ref, or any other resolver error degrades to "no
// digest for this service" (absent from the returned map) rather than
// failing the update or mutating any stack file, matching the opportunistic
// image-digest capture: registry info is a visibility nicety,
// never a gate. A canceled context short-circuits the remaining lookups, and
// a nil client returns an empty map.
// The result maps service name -> registry digest for the services whose
// candidate ref resolved cleanly to a non-empty digest.
func (e *Engine) resolveRegistryDigests(
	ctx context.Context,
	changes []updateServiceChange,
) map[string]string {
	digests := map[string]string{}
	client := e.newRegistryClient()
	if client == nil {
		return digests
	}
	for _, change := range changes {
		// A removed service (empty candidate ref) has no catalog-pinned tag to
		// resolve; skip it.
		if change.candidateRef == "" {
			continue
		}
		if ctx.Err() != nil {
			return digests
		}
		manifest, err := client.ResolveDigest(ctx, change.candidateRef)
		if err != nil {
			// Opportunistic: a registry-unreachable service degrades to no
			// disclosed digest; the plan is untouched (the invariant, the remaining case
			// #9). The error is swallowed — it never fails the update and never
			// reaches a sink.
			continue
		}
		if manifest.Digest != "" {
			digests[change.service] = manifest.Digest
		}
	}
	return digests
}

// Compile-time check that the production registry client satisfies the seam;
// both the fold-in and the explicit check depend only on ResolveDigest.
var _ RegistryResolver = (*registry.Client)(nil)
