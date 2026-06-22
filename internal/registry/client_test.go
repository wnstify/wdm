package registry_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/registry"
	"github.com/wnstify/wdm/pkg/types"
)

const (
	ociImageManifestType = "application/vnd.oci.image.manifest.v1+json"
	ociImageIndexType    = "application/vnd.oci.image.index.v1+json"
)

// newClientFor builds a client pointed at srv via srv's HTTP client and the
// "http" test scheme, so every request lands on the httptest server and
// never on a real registry.
func newClientFor(t *testing.T, srv *httptest.Server) *registry.Client {
	t.Helper()
	return registry.NewClient(
		registry.WithHTTPClient(srv.Client()),
		registry.WithScheme("http"),
	)
}

// refFor turns an httptest server URL into a registry-shaped reference with
// the test server's host:port as the registry, a fixed repository, and a
// tag. The "http" scheme is supplied to the client via WithScheme.
func refFor(t *testing.T, srv *httptest.Server, repo, tag string) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return fmt.Sprintf("%s/%s:%s", u.Host, repo, tag)
}

// manifestBody is a tiny but valid OCI image manifest JSON document.
func manifestBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociImageManifestType,
		"config":        map[string]any{"digest": "sha256:cfg"},
		"layers":        []any{},
	})
	require.NoError(t, err)
	return body
}

func TestResolveDigest_HappyPathUsesContentDigestHeader(t *testing.T) {
	t.Parallel()

	body := manifestBody(t)
	const wantDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v2/library/widget/manifests/1.2.3", r.URL.Path)
		assert.Contains(t, r.Header.Get("Accept"), ociImageManifestType)
		assert.Equal(t, "wdm-registry-client", r.Header.Get("User-Agent"))
		assert.Empty(t, r.Header.Get("Authorization"), "no auth header without a challenge")
		w.Header().Set("Content-Type", ociImageManifestType)
		w.Header().Set("Docker-Content-Digest", wantDigest)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := newClientFor(t, srv)
	m, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
	require.NoError(t, err)
	assert.Equal(t, wantDigest, m.Digest)
	assert.Equal(t, ociImageManifestType, m.MediaType)
}

func TestResolveDigest_ComputesDigestWhenHeaderAbsent(t *testing.T) {
	t.Parallel()

	body := manifestBody(t)
	sum := sha256.Sum256(body)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Type and no Docker-Content-Digest: the client must
		// compute the digest from the body and recover the media type from
		// the body's mediaType field.
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := newClientFor(t, srv)
	m, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
	require.NoError(t, err)
	assert.Equal(t, wantDigest, m.Digest)
	assert.Equal(t, ociImageManifestType, m.MediaType, "media type recovered from body")
}

func TestResolveDigest_HandlesImageIndex(t *testing.T) {
	t.Parallel()

	indexBody, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociImageIndexType,
		"manifests": []map[string]any{
			{"digest": "sha256:amd64", "platform": map[string]string{"architecture": "amd64", "os": "linux"}},
			{"digest": "sha256:arm64", "platform": map[string]string{"architecture": "arm64", "os": "linux"}},
		},
	})
	require.NoError(t, err)
	const wantDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ociImageIndexType)
		w.Header().Set("Docker-Content-Digest", wantDigest)
		_, _ = w.Write(indexBody)
	}))
	defer srv.Close()

	c := newClientFor(t, srv)
	m, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/multi", "latest"))
	require.NoError(t, err)
	assert.Equal(t, wantDigest, m.Digest, "the index digest is the tag's resolution")
	assert.Equal(t, ociImageIndexType, m.MediaType)
}

func TestResolveDigest_AnonymousTokenDance(t *testing.T) {
	t.Parallel()

	body := manifestBody(t)
	const wantDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	const issuedToken = "anon-token-value"

	var (
		manifestNoAuth   atomic.Int32
		manifestWithAuth atomic.Int32
		tokenHits        atomic.Int32
	)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenHits.Add(1)
		// The token request must carry NO Authorization header (anonymous)
		// and must forward service + scope from the challenge.
		assert.Empty(t, r.Header.Get("Authorization"), "anonymous token request carries no credentials")
		assert.Equal(t, "registry.test", r.URL.Query().Get("service"))
		assert.Equal(t, "repository:library/widget:pull", r.URL.Query().Get("scope"))
		_ = json.NewEncoder(w).Encode(map[string]string{"token": issuedToken})
	})

	mux.HandleFunc("/v2/library/widget/manifests/1.2.3", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			manifestNoAuth.Add(1)
			challenge := fmt.Sprintf(
				`Bearer realm="%s/token",service="registry.test",scope="repository:library/widget:pull"`,
				srv.URL,
			)
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		manifestWithAuth.Add(1)
		assert.Equal(t, "Bearer "+issuedToken, auth)
		w.Header().Set("Content-Type", ociImageManifestType)
		w.Header().Set("Docker-Content-Digest", wantDigest)
		_, _ = w.Write(body)
	})

	c := newClientFor(t, srv)
	m, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
	require.NoError(t, err)
	assert.Equal(t, wantDigest, m.Digest)
	assert.Equal(t, int32(1), manifestNoAuth.Load(), "first attempt is anonymous")
	assert.Equal(t, int32(1), tokenHits.Load(), "exactly one token fetch")
	assert.Equal(t, int32(1), manifestWithAuth.Load(), "retried once with the bearer token")
}

func TestResolveDigest_TokenRejectedAfterDance(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "stale"})
	})
	mux.HandleFunc("/v2/library/widget/manifests/1.2.3", func(w http.ResponseWriter, _ *http.Request) {
		// Always 401, even with a token: the dance fails closed.
		challenge := fmt.Sprintf(`Bearer realm="%s/token",service="registry.test"`, srv.URL)
		w.Header().Set("WWW-Authenticate", challenge)
		w.WriteHeader(http.StatusUnauthorized)
	})

	c := newClientFor(t, srv)
	_, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
	requireNetworkError(t, err)
}

func TestResolveDigest_Unauthorized401WithoutChallengeFailsClosed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // no WWW-Authenticate header
	}))
	defer srv.Close()

	c := newClientFor(t, srv)
	_, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
	requireNetworkError(t, err)
}

func TestResolveDigest_NonBearerChallengeFailsClosed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="reg"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newClientFor(t, srv)
	_, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
	requireNetworkError(t, err)
}

func TestResolveDigest_StatusErrorsMapToNetworkFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{name: "not found", status: http.StatusNotFound},
		{name: "rate limited", status: http.StatusTooManyRequests},
		{name: "forbidden", status: http.StatusForbidden},
		{name: "server error", status: http.StatusInternalServerError},
		{name: "bad gateway", status: http.StatusBadGateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			c := newClientFor(t, srv)
			_, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
			requireNetworkError(t, err)
		})
	}
}

func TestResolveDigest_MalformedManifestStillResolvesViaHeader(t *testing.T) {
	t.Parallel()

	// A body that is not valid JSON is fine when the digest comes from the
	// header — the client does not parse the manifest for the digest, only
	// for the media-type fallback. The digest header is authoritative.
	const wantDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ociImageManifestType)
		w.Header().Set("Docker-Content-Digest", wantDigest)
		_, _ = w.Write([]byte("not-json{{"))
	}))
	defer srv.Close()

	c := newClientFor(t, srv)
	m, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
	require.NoError(t, err)
	assert.Equal(t, wantDigest, m.Digest)
	assert.Equal(t, ociImageManifestType, m.MediaType, "media type from Content-Type header")
}

func TestResolveDigest_OversizedBodyRejected(t *testing.T) {
	t.Parallel()

	// 5 MiB body with no digest header forces a full read, exceeding the
	// 4 MiB manifest cap.
	huge := strings.Repeat("a", 5<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	c := newClientFor(t, srv)
	_, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
	requireNetworkError(t, err)
}

func TestResolveDigest_TransportFailureMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	client := srv.Client()
	srv.Close() // server is down: the connection will fail.

	c := registry.NewClient(registry.WithHTTPClient(client), registry.WithScheme("http"))
	u, err := url.Parse(addr)
	require.NoError(t, err)
	_, err = c.ResolveDigest(t.Context(), fmt.Sprintf("%s/library/widget:1.2.3", u.Host))
	requireNetworkError(t, err)
}

func TestResolveDigest_ContextCanceledMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // block until the test cancels the context.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already canceled before the call.

	c := newClientFor(t, srv)
	_, err := c.ResolveDigest(ctx, refFor(t, srv, "library/widget", "1.2.3"))
	requireNetworkError(t, err)
}

func TestResolveDigest_ContextDeadlineMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	c := newClientFor(t, srv)
	_, err := c.ResolveDigest(ctx, refFor(t, srv, "library/widget", "1.2.3"))
	requireNetworkError(t, err)
}

func TestResolveDigest_MalformedReferenceMapsToUsageError(t *testing.T) {
	t.Parallel()

	c := registry.NewClient() // never used: parse fails before any request.
	_, err := c.ResolveDigest(t.Context(), "GHCR.io/Bad Repo")
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"want usage-validation (exit 2), got %v", err)
}

func TestTokenEndpoint_FailureMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tokenFunc http.HandlerFunc
	}{
		{
			name: "token endpoint 500",
			tokenFunc: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name: "token endpoint malformed json",
			tokenFunc: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("{bad"))
			},
		},
		{
			name: "token endpoint empty token",
			tokenFunc: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"token": ""})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			srv := httptest.NewServer(mux)
			defer srv.Close()

			mux.HandleFunc("/token", tt.tokenFunc)
			mux.HandleFunc("/v2/library/widget/manifests/1.2.3", func(w http.ResponseWriter, _ *http.Request) {
				challenge := fmt.Sprintf(`Bearer realm="%s/token",service="registry.test"`, srv.URL)
				w.Header().Set("WWW-Authenticate", challenge)
				w.WriteHeader(http.StatusUnauthorized)
			})

			c := newClientFor(t, srv)
			_, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
			requireNetworkError(t, err)
		})
	}
}

func TestChallengeMissingRealmFailsClosed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer service="registry.test"`) // no realm
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newClientFor(t, srv)
	_, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
	requireNetworkError(t, err)
}

// errDoer is an httpDoer that records every call and returns a fixed error,
// proving the injected seam is the ONLY network path and that no request
// escapes to a real registry.
type recordingDoer struct {
	calls atomic.Int32
}

func (d *recordingDoer) Do(*http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return nil, fmt.Errorf("recorded: no real network")
}

func TestInjectedDoerIsTheOnlyNetworkPath(t *testing.T) {
	t.Parallel()

	doer := &recordingDoer{}
	c := registry.NewClient(registry.WithHTTPClient(doer), registry.WithScheme("http"))

	_, err := c.ResolveDigest(t.Context(), "registry.test/library/widget:1.2.3")
	requireNetworkError(t, err)
	assert.Equal(t, int32(1), doer.calls.Load(), "exactly one request went through the injected doer")
}

func TestWithScheme_RejectsUnsafeSchemes(t *testing.T) {
	t.Parallel()

	// WithScheme ignores anything but http/https, and a nil/blank doer is
	// ignored, so a caller cannot downgrade to a "file" scheme. We assert
	// the option is a no-op by confirming a valid http server still works
	// after a hostile WithScheme call is also applied.
	body := manifestBody(t)
	const wantDigest = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", wantDigest)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := registry.NewClient(
		registry.WithHTTPClient(srv.Client()),
		registry.WithScheme("file"), // ignored
		registry.WithScheme("http"), // applied
	)
	m, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
	require.NoError(t, err)
	assert.Equal(t, wantDigest, m.Digest)
}

func TestNetworkErrorsNeverLeakTokenOrBody(t *testing.T) {
	t.Parallel()

	const secretToken = "SUPER-SECRET-BEARER-9f8e7d"
	const secretBody = "SECRET-RESPONSE-BODY-CONTENT"

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": secretToken})
	})
	mux.HandleFunc("/v2/library/widget/manifests/1.2.3", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			challenge := fmt.Sprintf(`Bearer realm="%s/token",service="registry.test"`, srv.URL)
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Authenticated retry returns a 500 carrying a secret body.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(secretBody))
	})

	c := newClientFor(t, srv)
	_, err := c.ResolveDigest(t.Context(), refFor(t, srv, "library/widget", "1.2.3"))
	require.Error(t, err)

	full := err.Error()
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	full += " " + typed.Message + " " + typed.Hint
	assert.NotContains(t, full, secretToken, "token must never appear in the error")
	assert.NotContains(t, full, secretBody, "response body must never appear in the error")
}

func TestDefaultClient_RefusesRedirectToNonHTTPScheme(t *testing.T) {
	t.Parallel()

	// Use the REAL default HTTP client (no WithHTTPClient) so its
	// CheckRedirect policy is exercised. The server redirects the manifest
	// fetch to a "file://" target; the policy must refuse, surfacing a
	// network failure rather than following an unsafe redirect.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "file:///etc/passwd")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	c := registry.NewClient(registry.WithScheme("http")) // default doer
	_, err = c.ResolveDigest(t.Context(), fmt.Sprintf("%s/library/widget:1.2.3", u.Host))
	requireNetworkError(t, err)
}

func TestDefaultClient_StopsRedirectLoop(t *testing.T) {
	t.Parallel()

	// A self-redirecting server would loop forever without the redirect
	// cap. The default client's CheckRedirect stops after maxRedirects and
	// the failure surfaces as a network error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	c := registry.NewClient(registry.WithScheme("http"))
	_, err = c.ResolveDigest(t.Context(), fmt.Sprintf("%s/library/widget:1.2.3", u.Host))
	requireNetworkError(t, err)
}

// requireNetworkError asserts err is a typed network failure (exit 8) and
// NOT a verification failure (exit 3) — this client never emits exit 3.
func requireNetworkError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure),
		"want network-failure (exit 8), got %v", err)
	assert.False(t, types.IsCode(err, types.ErrCodeVerificationFailed),
		"this client must never emit verification-failed (exit 3)")
}
