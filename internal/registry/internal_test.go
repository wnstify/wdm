package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBearerChallenge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		challenge string
		wantOK    bool
		realm     string
		service   string
		scope     string
	}{
		{
			name:      "full quoted challenge",
			challenge: `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`,
			wantOK:    true,
			realm:     "https://auth.docker.io/token",
			service:   "registry.docker.io",
			scope:     "repository:library/nginx:pull",
		},
		{
			name:      "case-insensitive scheme",
			challenge: `bearer realm="https://auth.test/token"`,
			wantOK:    true,
			realm:     "https://auth.test/token",
		},
		{
			name:      "quoted scope with comma",
			challenge: `Bearer realm="https://auth.test/token",scope="repository:a:pull,repository:b:pull"`,
			wantOK:    true,
			realm:     "https://auth.test/token",
			scope:     "repository:a:pull,repository:b:pull",
		},
		{
			name:      "unquoted values",
			challenge: `Bearer realm=https://auth.test/token,service=reg.test`,
			wantOK:    true,
			realm:     "https://auth.test/token",
			service:   "reg.test",
		},
		{
			name:      "basic scheme rejected",
			challenge: `Basic realm="reg"`,
			wantOK:    false,
		},
		{
			name:      "empty challenge rejected",
			challenge: "",
			wantOK:    false,
		},
		{
			name:      "no scheme token rejected",
			challenge: "realm=x",
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			params, ok := parseBearerChallenge(tt.challenge)
			assert.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				return
			}
			require.NotNil(t, params)
			assert.Equal(t, tt.realm, params["realm"])
			assert.Equal(t, tt.service, params["service"])
			assert.Equal(t, tt.scope, params["scope"])
		})
	}
}

func TestMediaTypeFromContentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "bare", input: "application/vnd.oci.image.manifest.v1+json", want: "application/vnd.oci.image.manifest.v1+json"},
		{name: "with charset", input: "application/json; charset=utf-8", want: "application/json"},
		{name: "with spaces", input: "  application/json ; charset=utf-8 ", want: "application/json"},
		{name: "empty", input: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, mediaTypeFromContentType(tt.input))
		})
	}
}

func TestMediaTypeFromBody(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "application/vnd.oci.image.index.v1+json",
		mediaTypeFromBody([]byte(`{"mediaType":"application/vnd.oci.image.index.v1+json"}`)))
	assert.Empty(t, mediaTypeFromBody([]byte("not json")))
	assert.Empty(t, mediaTypeFromBody([]byte(`{"schemaVersion":2}`)))
}

func TestEscapeRepositoryPathPreservesSlashes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "library/nginx", escapeRepositoryPath("library/nginx"))
	assert.Equal(t, "org/team/component", escapeRepositoryPath("org/team/component"))
}
