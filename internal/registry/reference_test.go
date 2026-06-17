package registry_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/registry"
	"github.com/wnstify/wdm/pkg/types"
)

func TestParseReference_NormalizesAndAccepts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		registry string
		repo     string
		tag      string
	}{
		{
			name:     "docker hub single name defaults",
			input:    "nginx",
			registry: "registry-1.docker.io",
			repo:     "library/nginx",
			tag:      "latest",
		},
		{
			name:     "docker hub single name with tag",
			input:    "nginx:1.27",
			registry: "registry-1.docker.io",
			repo:     "library/nginx",
			tag:      "1.27",
		},
		{
			name:     "docker hub namespaced repository",
			input:    "louislam/uptime-kuma:1.23.16",
			registry: "registry-1.docker.io",
			repo:     "louislam/uptime-kuma",
			tag:      "1.23.16",
		},
		{
			name:     "ghcr with host and namespace",
			input:    "ghcr.io/owner/app:v2.0.0",
			registry: "ghcr.io",
			repo:     "owner/app",
			tag:      "v2.0.0",
		},
		{
			name:     "registry with port",
			input:    "registry.example.com:5000/team/app:edge",
			registry: "registry.example.com:5000",
			repo:     "team/app",
			tag:      "edge",
		},
		{
			name:     "localhost is a registry",
			input:    "localhost:5000/app:dev",
			registry: "localhost:5000",
			repo:     "app",
			tag:      "dev",
		},
		{
			name:     "deep repository path",
			input:    "ghcr.io/org/team/component:1.0",
			registry: "ghcr.io",
			repo:     "org/team/component",
			tag:      "1.0",
		},
		{
			name:     "trims surrounding whitespace",
			input:    "  nginx:stable  ",
			registry: "registry-1.docker.io",
			repo:     "library/nginx",
			tag:      "stable",
		},
		{
			name:     "underscore and dot separators in component",
			input:    "ghcr.io/my_org/my.app:1.0",
			registry: "ghcr.io",
			repo:     "my_org/my.app",
			tag:      "1.0",
		},
		{
			name:     "mixed-case tag is valid",
			input:    "ghcr.io/owner/app:V1.2-RC1",
			registry: "ghcr.io",
			repo:     "owner/app",
			tag:      "V1.2-RC1",
		},
		{
			name:     "double underscore separator",
			input:    "ghcr.io/owner/a__b:1.0",
			registry: "ghcr.io",
			repo:     "owner/a__b",
			tag:      "1.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ref, err := registry.ParseReference(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.registry, ref.Registry)
			assert.Equal(t, tt.repo, ref.Repository)
			assert.Equal(t, tt.tag, ref.Tag)
			assert.Equal(t, tt.registry+"/"+tt.repo, ref.Image())
		})
	}
}

func TestParseReference_RejectsMalformedAsUsageError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "whitespace only", input: "   "},
		{name: "digest pin", input: "nginx@sha256:abc123"},
		{name: "digest pin with tag", input: "nginx:1.0@sha256:abc123"},
		{name: "empty tag after colon", input: "nginx:"},
		{name: "uppercase repository", input: "ghcr.io/Owner/App:1.0"},
		{name: "trailing slash repository", input: "ghcr.io/owner/:1.0"},
		{name: "double slash repository", input: "ghcr.io/owner//app:1.0"},
		{name: "leading dash tag", input: "nginx:-bad"},
		{name: "leading dot tag", input: "nginx:.bad"},
		{name: "tag with slash", input: "nginx:bad/tag"},
		{name: "tag with space", input: "nginx:bad tag"},
		{name: "repository with traversal", input: "ghcr.io/owner/../app:1.0"},
		{name: "repository starting with separator", input: "ghcr.io/owner/.app:1.0"},
		{name: "repository with query injection", input: "ghcr.io/owner/app?x=1:1.0"},
		{name: "repository trailing separator", input: "ghcr.io/owner/app-:1.0"},
		{name: "repository tripled underscore", input: "ghcr.io/owner/a___b:1.0"},
		{name: "over length", input: "nginx:" + strings.Repeat("a", 5000)},
		{name: "tag over 128 chars", input: "nginx:" + strings.Repeat("a", 129)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := registry.ParseReference(tt.input)
			require.Error(t, err)
			assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
				"want usage-validation (exit 2), got %v", err)
			assert.False(t, types.IsCode(err, types.ErrCodeNetworkFailure))
		})
	}
}

func TestParseReference_TagBoundaryLengths(t *testing.T) {
	t.Parallel()

	maxTag := strings.Repeat("a", 128)
	ref, err := registry.ParseReference("nginx:" + maxTag)
	require.NoError(t, err)
	assert.Equal(t, maxTag, ref.Tag)
}
