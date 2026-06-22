package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wnstify/wdm/pkg/types"
)

// This file is the trusted-fetch transport surface for the catalog and
// binary self-update paths: a Go-native GitHub release metadata client
// that fetches the latest release manifest and downloads release assets
// It is the network half of the
// network-vs-verification split — it does NO trust verification and NO
// apply. The 0.2 primitives (checksum / signature / attestation)
// trust-verify the bytes it returns downstream, where a trust failure maps
// to ErrCodeVerificationFailed (exit 3).
// Every fault here — transport error, non-2xx status (including 404
// no-release and 403/429 rate-limit), JSON decode failure, body-read
// failure, size-cap exceeded, and ctx cancel/deadline — maps to a single
// typed [types.ErrCodeNetworkFailure] (exit 8) via [networkError]. The
// client NEVER emits [types.ErrCodeVerificationFailed]; that lives in the
// verifier. Messages name only the endpoint kind and HTTP status, never
// raw response bodies or header values, so no token/telemetry can leak
// (PRD §24 redaction spirit; the client is anonymous and carries no
// Authorization header).

const (
	// defaultBaseURL is the public GitHub REST API root. It is overridable
	// via [WithBaseURL] so tests point the client at an httptest server
	// rather than real GitHub ("Test through seams").
	defaultBaseURL = "https://api.github.com"

	// defaultRequestTimeout bounds the default HTTP client so a hung GitHub
	// endpoint cannot stall an operation indefinitely. A caller-supplied
	// context deadline still applies and wins when shorter.
	defaultRequestTimeout = 30 * time.Second

	// userAgent identifies the client to GitHub, which rejects requests
	// without a User-Agent. It carries no version or host detail beyond the
	// product name, so it leaks nothing about the caller.
	userAgent = "wdm-release-client"

	// acceptHeader is the GitHub-recommended Accept value pinning the v3
	// REST media type for metadata requests.
	acceptHeader = "application/vnd.github+json"

	// maxMetadataBytes bounds the release-metadata JSON body so a hostile
	// or misbehaving endpoint cannot exhaust memory through the metadata
	// path. Real releases/latest payloads are a few KiB; 8 MiB is generous.
	maxMetadataBytes = 8 << 20
)

// httpDoer is the injectable HTTP transport seam. The standard
// [*http.Client] satisfies it; tests inject a fake or httptest-backed
// client so no test touches real GitHub.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ReleaseAsset is one downloadable file attached to a GitHub release: the
// subset of the GitHub asset object the self-update paths need — the file
// name, its download URL, and its declared size. It carries no trust state;
// the bytes a download yields are verified downstream.
// The name keeps the "Release" prefix because this package already owns a
// distinct [Asset] type (the trust-contract asset in artifacts.go, {Name,
// Role}); the unqualified Asset is taken, so a release-listing asset needs
// a separate name.
//
//nolint:revive // Asset is already an unrelated trust-contract type in this package.
type ReleaseAsset struct {
	// Name is the asset file name (matched against the Artifact* contract
	// by the apply paths, not here).
	Name string `json:"name"`

	// DownloadURL is the asset's browser_download_url — the URL
	// [Client.DownloadAsset] GETs to fetch the bytes.
	DownloadURL string `json:"browser_download_url"`
}

// Metadata is the parsed subset of a GitHub release object: its tag and
// its asset list. It is plain data with no trust or network state.
type Metadata struct {
	// Tag is the release tag (the GitHub tag_name), e.g. "v1.2.3".
	Tag string `json:"tag_name"`

	// Assets is the release's downloadable asset list.
	Assets []ReleaseAsset `json:"assets"`
}

// FindAsset returns the asset with the given name and true, or a zero
// asset and false when no asset matches. It is an exact-name lookup the
// apply paths use to resolve the binary, checksum, and signature assets
// from the Artifact* contract. It does not enforce that contract; a missing
// asset is the caller's decision to act on.
func (m Metadata) FindAsset(name string) (ReleaseAsset, bool) {
	for _, asset := range m.Assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return ReleaseAsset{}, false
}

// Client fetches GitHub release metadata and downloads release assets over
// an injectable HTTP seam. It is anonymous — it sends no Authorization
// header and stores no credentials. Every failure maps to
// [types.ErrCodeNetworkFailure]; the client performs no verification.
// Construct a Client with [NewClient] and configure it with the functional
// options ([WithHTTPClient], [WithBaseURL]). A zero Client is not usable; go
// through the constructor.
type Client struct {
	doer    httpDoer
	baseURL string
	owner   string
	repo    string
}

// Option configures a [Client] at construction time.
type Option func(*Client)

// WithHTTPClient injects the HTTP transport the client uses. Tests pass an
// httptest-backed [*http.Client] (or any [httpDoer]) so no request reaches
// real GitHub. A nil doer is ignored, leaving the constructor's bounded
// default client in place.
func WithHTTPClient(doer httpDoer) Option {
	return func(c *Client) {
		if doer != nil {
			c.doer = doer
		}
	}
}

// WithBaseURL overrides the GitHub REST API base URL (default
// https://api.github.com). Tests point this at an httptest server. A
// trailing slash is trimmed so endpoint paths join cleanly. A blank value
// is ignored, leaving the default base URL in place.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
		if trimmed != "" {
			c.baseURL = trimmed
		}
	}
}

// NewClient builds a release metadata client for the repository named by
// policy.SourceRepository (owner/name). The default HTTP client carries a
// bounded timeout ([defaultRequestTimeout]); [WithHTTPClient] replaces it
// for tests. The default base URL is the public GitHub REST API;
// [WithBaseURL] overrides the endpoint. The owner/name identity is derived
// from policy.SourceRepository.
// It returns a typed [types.ErrCodeNetworkFailure] when no usable
// repository identity remains — policy.SourceRepository did not produce a
// valid owner/name pair — because a client with no target cannot make a
// request.
func NewClient(policy TrustPolicy, opts ...Option) (*Client, error) {
	owner, repo := splitOwnerRepo(policy.SourceRepository)

	c := &Client{
		doer:    &http.Client{Timeout: defaultRequestTimeout},
		baseURL: defaultBaseURL,
		owner:   owner,
		repo:    repo,
	}
	for _, opt := range opts {
		opt(c)
	}

	if c.owner == "" || c.repo == "" {
		return nil, networkError(
			"release client has no target repository",
			"a valid owner/name repository identity is required",
			nil,
		)
	}

	return c, nil
}

// LatestRelease fetches and parses the latest release metadata for the
// configured repository: GET <base>/repos/<owner>/<repo>/releases/latest.
// The request carries the GitHub Accept and User-Agent headers and honors
// ctx for cancellation and deadlines.
// Every failure — request build, transport error, non-2xx status
// (including 404 when no release exists and 403/429 rate-limit), body-read
// failure, or JSON decode failure — returns a typed
// [types.ErrCodeNetworkFailure] (exit 8). It performs no verification; the
// returned metadata is trust-verified downstream.
func (c *Client) LatestRelease(ctx context.Context) (*Metadata, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/releases/latest", c.baseURL, c.owner, c.repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, networkError("building the release metadata request failed", "", err)
	}
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("User-Agent", userAgent)

	body, err := c.fetch(req, "release metadata", maxMetadataBytes)
	if err != nil {
		return nil, err
	}

	var meta Metadata
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, networkError(
			"parsing the release metadata response failed",
			"GitHub returned an unexpected response shape",
			err,
		)
	}

	return &meta, nil
}

// DownloadAsset fetches the bytes of asset from its download URL, capped
// at maxBytes. It GETs asset.DownloadURL, streams the body through an
// [io.LimitReader] bounded one byte past maxBytes, and rejects a response
// over the cap rather than return a truncated payload. It carries the
// User-Agent header and honors ctx.
// The returned bytes are raw and UNVERIFIED — the caller passes them to
// the 0.2 verifier (checksum, then signature) before any apply. Every
// failure — blank URL, non-positive cap, request build, transport error,
// non-2xx status, body-read failure, or an over-cap body — returns a typed
// [types.ErrCodeNetworkFailure] (exit 8). It never emits a verification
// error.
func (c *Client) DownloadAsset(ctx context.Context, asset ReleaseAsset, maxBytes int64) ([]byte, error) {
	if strings.TrimSpace(asset.DownloadURL) == "" {
		return nil, networkError("release asset has no download url", "", nil)
	}
	if maxBytes <= 0 {
		return nil, networkError("release asset download size cap must be positive", "", nil)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.DownloadURL, nil)
	if err != nil {
		return nil, networkError("building the release asset request failed", "", err)
	}
	req.Header.Set("User-Agent", userAgent)

	return c.fetch(req, "release asset", maxBytes)
}

// fetch executes req, validates a 2xx status, and reads the body capped at
// maxBytes. kind is a short, low-cardinality label ("release metadata" /
// "release asset") that appears in error messages so failures are
// attributable without leaking the endpoint URL or any response content.
// It maps every fault to [types.ErrCodeNetworkFailure]: transport errors
// (carrying ctx cancellation and deadline as their cause), non-2xx statuses
// (rate-limit statuses get a clearer message), an over-cap body, and
// body-read failures.
func (c *Client) fetch(req *http.Request, kind string, maxBytes int64) ([]byte, error) {
	resp, err := c.doer.Do(req)
	if err != nil {
		// Transport failures wrap ctx.Canceled / context.DeadlineExceeded
		// when the request was canceled; keep the cause for errors.Is while
		// the message stays generic.
		return nil, networkError(
			fmt.Sprintf("%s request failed", kind),
			"check the network connection and try again",
			err,
		)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close of a drained response body

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, statusError(kind, resp.StatusCode)
	}

	// Read one byte past the cap so an exactly-at-cap body still reads
	// fully while an over-cap body stays detectable.
	limited := io.LimitReader(resp.Body, maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, networkError(
			fmt.Sprintf("reading the %s response failed", kind),
			"",
			err,
		)
	}
	if int64(len(body)) > maxBytes {
		return nil, networkError(
			fmt.Sprintf("%s response exceeds the size limit", kind),
			"the download is larger than allowed and was refused",
			errBodyTooLarge,
		)
	}

	return body, nil
}

// statusError maps a non-2xx HTTP status to a typed network failure.
// GitHub signals rate limiting with 403 (secondary limit / quota
// exhausted) and 429 (primary limit); both get a clear "rate limit"
// message and a wait-and-retry hint. Every other non-2xx status names only
// the kind and the numeric code — never the response body — so nothing from
// the wire leaks into the surfaced error.
func statusError(kind string, status int) error {
	if status == http.StatusTooManyRequests || status == http.StatusForbidden {
		return networkError(
			fmt.Sprintf("github rate limit reached fetching %s", kind),
			"wait for the GitHub rate limit to reset and try again",
			nil,
		)
	}
	return networkError(
		fmt.Sprintf("%s request returned http status %d", kind, status),
		"",
		nil,
	)
}

// splitOwnerRepo splits an "owner/name" identity into its parts. A value
// that is not exactly one non-empty owner and one non-empty name yields
// two empty strings, which [NewClient] rejects.
func splitOwnerRepo(sourceRepository string) (owner, repo string) {
	owner, repo, found := strings.Cut(strings.TrimSpace(sourceRepository), "/")
	if !found {
		return "", ""
	}
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return "", ""
	}
	return owner, repo
}

// errBodyTooLarge is the cause attached when a response exceeds its size
// cap, so callers can branch on the size-cap failure via errors.Is while
// the surfaced *Error stays a generic network failure.
var errBodyTooLarge = errors.New("release: response exceeds size cap")

// networkError wraps a fail-closed network fault with the
// [types.ErrCodeNetworkFailure] exit code. It is
// the single error constructor for every fault in the release metadata
// client, so transport, status, decode, size-cap, and cancellation
// failures all map to one exit code regardless of which step rejected
// It never produces an [types.ErrCodeVerificationFailed];
// trust failures live in the verifier.
func networkError(message, hint string, cause error) error {
	if cause != nil {
		return types.WrapError(types.ErrCodeNetworkFailure, message, hint, cause)
	}
	return types.NewError(types.ErrCodeNetworkFailure, message, hint)
}
