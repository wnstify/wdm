package release_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/release"
)

func TestDefaultTrustPolicy_PinsPublicWnstifyAnchors(t *testing.T) {
	t.Parallel()

	policy := release.DefaultTrustPolicy()

	// Issuer is exactly the GitHub Actions OIDC issuer — not a guessed or
	// empty value (PRD §12: prefer keyless signing through GitHub Actions
	// OIDC).
	assert.Equal(t, "https://token.actions.githubusercontent.com", policy.OIDCIssuer)

	// Repository identity is exactly the public wnstify/wdm (the invariant,
	assert.Equal(t, "https://github.com/wnstify/wdm", policy.RepositoryURL)
	assert.Equal(t, "wnstify/wdm", policy.SourceRepository)

	// Release workflow identity is the single release workflow file.
	assert.Equal(t, ".github/workflows/release.yml", policy.ReleaseWorkflowPath)

	// The keyless certificate-identity (SAN) prefix for tag releases is the
	// repo URL + workflow path joined by "@refs/tags/"; the verifier
	// appends the concrete <tag> in.2.
	assert.Equal(
		t,
		"https://github.com/wnstify/wdm/.github/workflows/release.yml@refs/tags/",
		policy.TagCertificateIdentityPrefix,
	)
}

func TestExportedConstants_MatchPolicyFields(t *testing.T) {
	t.Parallel()

	// The exported constants are the source of truth that SECURITY.md
	// quotes; the constructor must project them verbatim so docs and
	// policy cannot drift.
	policy := release.DefaultTrustPolicy()

	assert.Equal(t, release.OIDCIssuer, policy.OIDCIssuer)
	assert.Equal(t, release.RepositoryURL, policy.RepositoryURL)
	assert.Equal(t, release.SourceRepository, policy.SourceRepository)
	assert.Equal(t, release.ReleaseWorkflowPath, policy.ReleaseWorkflowPath)
}

func TestDefaultTrustPolicy_NoTemporaryPrivateRepoIdentity(t *testing.T) {
	t.Parallel()

	// The temporary private repo name must never leak into the pinned
	// trust policy (the invariant: pin the final wnstify/wdm anchors from
	// the start). Assert across every string field.
	policy := release.DefaultTrustPolicy()

	fields := map[string]string{
		"OIDCIssuer":                   policy.OIDCIssuer,
		"RepositoryURL":                policy.RepositoryURL,
		"SourceRepository":             policy.SourceRepository,
		"ReleaseWorkflowPath":          policy.ReleaseWorkflowPath,
		"TagCertificateIdentityPrefix": policy.TagCertificateIdentityPrefix,
	}

	for name, value := range fields {
		require.NotContains(
			t,
			value,
			"wn-docker-manager",
			"field %s must not carry the temporary private repo name", name,
		)
	}
}

func TestDefaultTrustPolicy_TagPrefixComposedFromAnchors(t *testing.T) {
	t.Parallel()

	// The SAN prefix is derived from the repo URL and workflow path, so it
	// must contain both and end at the tag-ref boundary the verifier
	// completes.
	policy := release.DefaultTrustPolicy()

	assert.True(
		t,
		strings.HasPrefix(policy.TagCertificateIdentityPrefix, policy.RepositoryURL+"/"),
		"SAN prefix must start with the repository URL",
	)
	assert.Contains(t, policy.TagCertificateIdentityPrefix, policy.ReleaseWorkflowPath)
	assert.True(
		t,
		strings.HasSuffix(policy.TagCertificateIdentityPrefix, "@refs/tags/"),
		"SAN prefix must end at the tag-ref boundary",
	)
}
