package release_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/release"
	"github.com/wnstify/wdm/pkg/types"
)

// The shared testPolicy helper (attestation_test.go) returns
// release.DefaultTrustPolicy, whose SourceRepository is "wnstify/wdm" —
// the owner/repo every request path below is asserted against.

// newTestClient builds a client pointed at srv with srv's HTTP client, so
// every request lands on the httptest server and never on real GitHub.
func newTestClient(t *testing.T, srv *httptest.Server) *release.Client {
	t.Helper()

	c, err := release.NewClient(
		testPolicy(),
		release.WithBaseURL(srv.URL),
		release.WithHTTPClient(srv.Client()),
	)
	require.NoError(t, err)
	require.NotNil(t, c)

	return c
}

// latestReleaseBody is a canned GitHub releases/latest JSON payload with
// the fields the client parses plus extra fields it must ignore.
func latestReleaseBody(t *testing.T) []byte {
	t.Helper()

	payload := map[string]any{
		"tag_name":   "v1.2.3",
		"name":       "ignored display name",
		"draft":      false,
		"prerelease": false,
		"assets": []map[string]any{
			{
				"name":                 "wdm-linux-amd64",
				"browser_download_url": "https://example.test/download/wdm-linux-amd64",
				"size":                 1234,
				"content_type":         "application/octet-stream",
			},
			{
				"name":                 "SHA256SUMS",
				"browser_download_url": "https://example.test/download/SHA256SUMS",
				"size":                 256,
			},
		},
	}

	body, err := json.Marshal(payload)
	require.NoError(t, err)

	return body
}

func TestNewClient_RejectsMissingRepository(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy release.TrustPolicy
		opts   []release.Option
	}{
		{
			name:   "blank source repository",
			policy: release.TrustPolicy{SourceRepository: ""},
		},
		{
			name:   "no slash in source repository",
			policy: release.TrustPolicy{SourceRepository: "wnstify"},
		},
		{
			name:   "empty owner half",
			policy: release.TrustPolicy{SourceRepository: "/wdm"},
		},
		{
			name:   "empty repo half",
			policy: release.TrustPolicy{SourceRepository: "wnstify/"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c, err := release.NewClient(tt.policy, tt.opts...)
			assert.Nil(t, c)
			require.Error(t, err)
			assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure))
		})
	}
}

func TestNewClient_WithRepositoryOverridesPolicy(t *testing.T) {
	t.Parallel()

	var gotPath atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		gotPath.Store(&path)
		_, _ = w.Write(latestReleaseBody(t))
	}))
	t.Cleanup(srv.Close)

	c, err := release.NewClient(
		release.TrustPolicy{SourceRepository: "ignored/policy"},
		release.WithBaseURL(srv.URL),
		release.WithHTTPClient(srv.Client()),
		release.WithRepository("override-owner", "override-repo"),
	)
	require.NoError(t, err)

	_, err = c.LatestRelease(context.Background())
	require.NoError(t, err)

	require.NotNil(t, gotPath.Load())
	assert.Equal(t, "/repos/override-owner/override-repo/releases/latest", *gotPath.Load())
}

func TestLatestRelease_ParsesTagAndAssets(t *testing.T) {
	t.Parallel()

	var (
		gotPath      atomic.Pointer[string]
		gotUserAgent atomic.Pointer[string]
		gotAccept    atomic.Pointer[string]
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		ua := r.Header.Get("User-Agent")
		accept := r.Header.Get("Accept")
		gotPath.Store(&path)
		gotUserAgent.Store(&ua)
		gotAccept.Store(&accept)
		_, _ = w.Write(latestReleaseBody(t))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)

	meta, err := c.LatestRelease(context.Background())
	require.NoError(t, err)
	require.NotNil(t, meta)

	assert.Equal(t, "v1.2.3", meta.Tag)
	require.Len(t, meta.Assets, 2)
	assert.Equal(t, "wdm-linux-amd64", meta.Assets[0].Name)
	assert.Equal(t, "https://example.test/download/wdm-linux-amd64", meta.Assets[0].DownloadURL)

	// Outbound request shape: path, User-Agent (GitHub requires one), and
	// the GitHub media-type Accept header.
	require.NotNil(t, gotPath.Load())
	assert.Equal(t, "/repos/wnstify/wdm/releases/latest", *gotPath.Load())
	require.NotNil(t, gotUserAgent.Load())
	assert.NotEmpty(t, *gotUserAgent.Load())
	require.NotNil(t, gotAccept.Load())
	assert.Equal(t, "application/vnd.github+json", *gotAccept.Load())
}

func TestMetadata_FindAsset(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(latestReleaseBody(t))
	}))
	t.Cleanup(srv.Close)

	meta, err := newTestClient(t, srv).LatestRelease(context.Background())
	require.NoError(t, err)

	found, ok := meta.FindAsset("SHA256SUMS")
	assert.True(t, ok)
	assert.Equal(t, "SHA256SUMS", found.Name)
	assert.Equal(t, "https://example.test/download/SHA256SUMS", found.DownloadURL)

	missing, ok := meta.FindAsset("does-not-exist")
	assert.False(t, ok)
	assert.Equal(t, release.ReleaseAsset{}, missing)
}

func TestLatestRelease_NonSuccessStatusMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		wantSubstr string // lowercased message fragment that must appear
	}{
		{name: "404 no release", status: http.StatusNotFound, wantSubstr: "http status 404"},
		{name: "403 rate limit", status: http.StatusForbidden, wantSubstr: "rate limit"},
		{name: "429 too many requests", status: http.StatusTooManyRequests, wantSubstr: "rate limit"},
		{name: "500 server error", status: http.StatusInternalServerError, wantSubstr: "http status 500"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// A body is written deliberately to prove it is never
				// surfaced in the error message.
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"message":"secret-body-should-not-leak"}`))
			}))
			t.Cleanup(srv.Close)

			meta, err := newTestClient(t, srv).LatestRelease(context.Background())
			assert.Nil(t, meta)
			require.Error(t, err)
			assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure))

			msg := strings.ToLower(err.Error())
			assert.Contains(t, msg, tt.wantSubstr)
			assert.NotContains(t, msg, "secret-body-should-not-leak")
		})
	}
}

func TestLatestRelease_MalformedJSONMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not valid json"))
	}))
	t.Cleanup(srv.Close)

	meta, err := newTestClient(t, srv).LatestRelease(context.Background())
	assert.Nil(t, meta)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure))
}

func TestLatestRelease_TransportFailureMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	// Start a server, capture its URL/client, then close it so the next
	// request fails at the transport layer (connection refused).
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c := newTestClient(t, srv)
	srv.Close()

	meta, err := c.LatestRelease(context.Background())
	assert.Nil(t, meta)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure))
}

func TestLatestRelease_ContextCanceledMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(latestReleaseBody(t))
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before the request is issued

	meta, err := newTestClient(t, srv).LatestRelease(ctx)
	assert.Nil(t, meta)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure))
	assert.ErrorIs(t, err, context.Canceled)
}

func TestLatestRelease_ContextDeadlineDuringRequestMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client's deadline fires (or the test unblocks it),
		// proving the request actually carries and honors ctx.
		select {
		case <-r.Context().Done():
		case <-unblock:
		}
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(func() {
		close(unblock)
		srv.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	meta, err := newTestClient(t, srv).LatestRelease(ctx)
	assert.Nil(t, meta)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestDownloadAsset_ReturnsExactBytes(t *testing.T) {
	t.Parallel()

	payload := []byte("the exact asset bytes the verifier will check")

	var gotUserAgent atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		gotUserAgent.Store(&ua)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	asset := release.ReleaseAsset{
		Name:        "wdm-linux-amd64",
		DownloadURL: srv.URL + "/download/wdm-linux-amd64",
	}

	got, err := c.DownloadAsset(context.Background(), asset, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	require.NotNil(t, gotUserAgent.Load())
	assert.NotEmpty(t, *gotUserAgent.Load())
}

func TestDownloadAsset_AtCapBoundaryReturnsAllBytes(t *testing.T) {
	t.Parallel()

	payload := []byte("0123456789") // exactly 10 bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	asset := release.ReleaseAsset{DownloadURL: srv.URL + "/asset"}
	got, err := newTestClient(t, srv).DownloadAsset(context.Background(), asset, int64(len(payload)))
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestDownloadAsset_OverCapMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this body is longer than the cap"))
	}))
	t.Cleanup(srv.Close)

	asset := release.ReleaseAsset{DownloadURL: srv.URL + "/asset"}
	got, err := newTestClient(t, srv).DownloadAsset(context.Background(), asset, 4)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure))
	assert.Contains(t, strings.ToLower(err.Error()), "size limit")
}

func TestDownloadAsset_NonSuccessStatusMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	asset := release.ReleaseAsset{DownloadURL: srv.URL + "/missing"}
	got, err := newTestClient(t, srv).DownloadAsset(context.Background(), asset, 1<<20)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure))
}

func TestDownloadAsset_RejectsBlankURLAndNonPositiveCap(t *testing.T) {
	t.Parallel()

	// No server is needed: both refusals happen before any request.
	c, err := release.NewClient(testPolicy())
	require.NoError(t, err)

	tests := []struct {
		name    string
		asset   release.ReleaseAsset
		maxByte int64
	}{
		{
			name:    "blank download url",
			asset:   release.ReleaseAsset{DownloadURL: "   "},
			maxByte: 1 << 20,
		},
		{
			name:    "zero cap",
			asset:   release.ReleaseAsset{DownloadURL: "https://example.test/asset"},
			maxByte: 0,
		},
		{
			name:    "negative cap",
			asset:   release.ReleaseAsset{DownloadURL: "https://example.test/asset"},
			maxByte: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := c.DownloadAsset(context.Background(), tt.asset, tt.maxByte)
			assert.Nil(t, got)
			require.Error(t, err)
			assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure))
		})
	}
}

func TestDownloadAsset_ContextCanceledMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	asset := release.ReleaseAsset{DownloadURL: srv.URL + "/asset"}
	got, err := newTestClient(t, srv).DownloadAsset(ctx, asset, 1<<20)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure))
	assert.ErrorIs(t, err, context.Canceled)
}

// fakeDoer is a minimal httpDoer-shaped double proving the HTTP seam is
// injectable through WithHTTPClient without an httptest server. It records
// the request it received and returns a canned response or error.
type fakeDoer struct {
	gotReq atomic.Pointer[http.Request]
	resp   *http.Response
	err    error
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.gotReq.Store(req)
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func TestLatestRelease_UsesInjectedDoerSeam(t *testing.T) {
	t.Parallel()

	doer := &fakeDoer{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v9.9.9","assets":[]}`)),
			Header:     make(http.Header),
		},
	}

	c, err := release.NewClient(
		testPolicy(),
		release.WithBaseURL("https://api.github.test"),
		release.WithHTTPClient(doer),
	)
	require.NoError(t, err)

	meta, err := c.LatestRelease(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "v9.9.9", meta.Tag)

	gotReq := doer.gotReq.Load()
	require.NotNil(t, gotReq)
	assert.Equal(t, "https://api.github.test/repos/wnstify/wdm/releases/latest", gotReq.URL.String())
	assert.Equal(t, "application/vnd.github+json", gotReq.Header.Get("Accept"))
	assert.NotEmpty(t, gotReq.Header.Get("User-Agent"))
}

// errReadCloser fails on Read to exercise the body-read failure path.
type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, fmt.Errorf("synthetic read error") }
func (errReadCloser) Close() error             { return nil }

func TestLatestRelease_BodyReadFailureMapsToNetworkFailure(t *testing.T) {
	t.Parallel()

	doer := &fakeDoer{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       errReadCloser{},
			Header:     make(http.Header),
		},
	}

	c, err := release.NewClient(
		testPolicy(),
		release.WithBaseURL("https://api.github.test"),
		release.WithHTTPClient(doer),
	)
	require.NoError(t, err)

	meta, err := c.LatestRelease(context.Background())
	assert.Nil(t, meta)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure))
}
