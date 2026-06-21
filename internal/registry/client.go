package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wnstify/wdm/pkg/types"
)

const (
	// defaultRequestTimeout bounds the default HTTP client so a hung
	// registry endpoint cannot stall an update check indefinitely. A
	// caller-supplied context deadline still applies on top of this and
	// wins when shorter.
	defaultRequestTimeout = 30 * time.Second

	// userAgent identifies the client to registries (some reject requests
	// without a User-Agent). It carries no version or host detail, so it
	// leaks nothing about the caller.
	userAgent = "wdm-registry-client"

	// maxManifestBytes bounds the manifest JSON body. Real OCI manifests
	// and image indexes are a few KiB; 4 MiB is generous and stops a
	// hostile or misbehaving registry from exhausting memory.
	maxManifestBytes = 4 << 20

	// maxTokenBytes bounds the token-endpoint JSON body. Token responses
	// are tiny; 1 MiB is far beyond any legitimate token document.
	maxTokenBytes = 1 << 20

	// maxRedirects bounds how many redirects the HTTP client follows so a
	// redirect loop cannot hang the check. Registries legitimately
	// redirect manifest/blob fetches to a CDN.
	maxRedirects = 10

	// dockerContentDigestHeader is the header registries set to the
	// canonical manifest digest. When present it is authoritative and the
	// client trusts it over a locally computed digest.
	dockerContentDigestHeader = "Docker-Content-Digest"
)

// acceptManifestMediaTypes is the Accept header value advertising every
// manifest and index media type the client understands, so a registry
// returns the manifest (or a manifest list / image index) rather than a
// legacy v1 document. Most-preferred first.
var acceptManifestMediaTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// httpDoer is the injectable HTTP transport seam. The standard
// [*http.Client] satisfies it; tests inject an httptest-backed client so
// no test touches a real registry ("Test through seams").
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Manifest is the resolved metadata for one image tag: the canonical
// manifest digest and the media type the registry reported. It carries no
// trust state — this client does no verification (a registry-reported digest
// is metadata, not proof).
type Manifest struct {
	// Digest is the canonical manifest digest, e.g. "sha256:abc123...". It is
	// the registry's Docker-Content-Digest header when present, else the
	// SHA-256 the client computes over the manifest body.
	Digest string

	// MediaType is the manifest media type the registry returned (the
	// Content-Type), e.g. an image manifest or a manifest list / index.
	MediaType string
}

// Client resolves image metadata from an OCI / Docker Registry v2 endpoint
// over an injectable HTTP seam. It is anonymous — it sends no caller-supplied
// Authorization header and stores no credentials — and runs the
// anonymous Bearer token dance on a 401 challenge. Every failure maps to
// [types.ErrCodeNetworkFailure] except a malformed caller reference, which
// maps to [types.ErrCodeUsageValidation]; the client performs no trust
// verification.
// Construct a Client with [NewClient] and the functional options
// ([WithHTTPClient], [WithScheme]). A zero Client is not usable — go through
// the constructor.
type Client struct {
	doer   httpDoer
	scheme string
}

// Option configures a [Client] at construction time.
type Option func(*Client)

// WithHTTPClient injects the HTTP transport the client uses. Tests pass an
// httptest-backed [*http.Client] (or any [httpDoer]) so no request reaches
// a real registry. A nil doer is ignored so the constructor's bounded,
// redirect-capped default client stays in place.
func WithHTTPClient(doer httpDoer) Option {
	return func(c *Client) {
		if doer != nil {
			c.doer = doer
		}
	}
}

// WithScheme overrides the URL scheme the client uses to reach the registry
// (default "https"). It exists ONLY so tests can point the client at an
// httptest server, whose URL is "http". A blank value, or anything other than
// "http" or "https", is ignored so production stays HTTPS — a caller cannot
// downgrade a real registry check to cleartext.
func WithScheme(scheme string) Option {
	return func(c *Client) {
		if scheme == "http" || scheme == "https" {
			c.scheme = scheme
		}
	}
}

// NewClient builds an anonymous registry metadata client. The default HTTP
// client carries a bounded timeout ([defaultRequestTimeout]) and a capped
// redirect policy that refuses to follow a redirect to a non-HTTP(S)
// scheme; [WithHTTPClient] replaces it for tests. The default scheme is
// HTTPS; [WithScheme] overrides it only for tests.
func NewClient(opts ...Option) *Client {
	c := &Client{
		doer:   defaultHTTPClient(),
		scheme: "https",
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// defaultHTTPClient builds the production HTTP client: a bounded total
// timeout plus a redirect policy that caps redirect depth and refuses any
// redirect whose target is not HTTP(S), so a hostile registry cannot redirect
// a metadata fetch to a "file://" or other unsafe scheme.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: defaultRequestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("registry: stopped after %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("registry: refusing redirect to non-http scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

// ResolveDigest resolves ref's tag to its canonical manifest digest via a
// registry v2 manifest request. It parses and validates ref first (a
// malformed reference is a usage error), runs the anonymous token dance on a
// 401 challenge, and returns the [Manifest] carrying the digest and media
// type. Manifest lists / image indexes are handled: the digest of the index
// itself is returned, the correct "what does this tag resolve to" answer for
// update detection.
// Every transport, status, token, size-cap, or decode failure returns a typed
// [types.ErrCodeNetworkFailure] (exit 8); a malformed ref returns
// [types.ErrCodeUsageValidation] (exit 2). It honors ctx for cancellation and
// deadlines and performs no verification.
func (c *Client) ResolveDigest(ctx context.Context, ref string) (Manifest, error) {
	parsed, err := ParseReference(ref)
	if err != nil {
		return Manifest{}, err
	}
	return c.resolveManifest(ctx, parsed)
}

// resolveManifest performs the v2 manifest GET for an already-parsed
// reference, including the anonymous token retry, and extracts the digest
// and media type.
func (c *Client) resolveManifest(ctx context.Context, ref Reference) (Manifest, error) {
	endpoint := c.manifestURL(ref)

	result, err := c.getWithToken(ctx, ref, endpoint, acceptManifestMediaTypes, maxManifestBytes)
	if err != nil {
		return Manifest{}, err
	}

	digest := strings.TrimSpace(result.contentDigest)
	if digest == "" {
		// The registry did not supply the canonical digest; compute it as
		// SHA-256 over the exact bytes the registry served.
		sum := sha256.Sum256(result.body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}

	// Prefer a manifest-shaped Content-Type header; otherwise recover the
	// media type from the body's declared mediaType field. A generic or
	// sniffed Content-Type (e.g. text/plain) that some registries and proxies
	// return is ignored in favor of the authoritative in-body value.
	mediaType := mediaTypeFromContentType(result.contentType)
	if !isManifestMediaType(mediaType) {
		if fromBody := mediaTypeFromBody(result.body); fromBody != "" {
			mediaType = fromBody
		}
	}

	return Manifest{Digest: digest, MediaType: mediaType}, nil
}

// httpResult is the outcome of a single registry GET. It carries only the
// fields the client needs — body bytes, status, the two header values, and
// the 401 challenge — so the live *http.Response (and its body) never crosses
// a function boundary unclosed. do reads and closes the response body before
// returning this.
type httpResult struct {
	body          []byte
	status        int
	contentType   string
	contentDigest string
	challenge     string // WWW-Authenticate value, set only on a 401.
}

// getWithToken performs a GET against endpoint with the given Accept value,
// reading at most maxBytes. On a 401 with a Bearer WWW-Authenticate
// challenge it fetches an anonymous token from the challenge realm and
// retries the request ONCE with the bearer token attached, returning the
// successful result. Every fault — including a 401 that cannot be satisfied
// anonymously — maps to a network failure.
func (c *Client) getWithToken(
	ctx context.Context,
	ref Reference,
	endpoint, accept string,
	maxBytes int64,
) (httpResult, error) {
	result, err := c.do(ctx, endpoint, accept, "", maxBytes)
	if err != nil {
		return httpResult{}, err
	}
	if result.status != http.StatusUnauthorized {
		return result, nil
	}

	// 401: try the anonymous token dance once. A challenge we cannot parse
	// as a Bearer realm/service/scope, or a registry that demands real
	// credentials, fails closed as a network failure.
	if result.challenge == "" {
		return httpResult{}, networkError(
			"the registry requires authentication",
			"this client performs anonymous public checks only",
			nil,
		)
	}

	token, err := c.fetchAnonymousToken(ctx, ref, result.challenge)
	if err != nil {
		return httpResult{}, err
	}

	result, err = c.do(ctx, endpoint, accept, token, maxBytes)
	if err != nil {
		return httpResult{}, err
	}
	if result.status == http.StatusUnauthorized {
		return httpResult{}, networkError(
			"the registry rejected the anonymous token",
			"this client performs anonymous public checks only",
			nil,
		)
	}

	return result, nil
}

// do executes a single GET against endpoint with the Accept header and an
// optional bearer token, reads the body capped at maxBytes, closes the
// response body, and returns an httpResult. A non-2xx, non-401 status maps to
// a network error here; a 401 is returned (with the WWW-Authenticate
// challenge) so the caller can run the token dance.
func (c *Client) do(ctx context.Context, endpoint, accept, token string, maxBytes int64) (httpResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return httpResult{}, networkError("building the registry request failed", "", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	httpResp, err := c.doer.Do(req)
	if err != nil {
		// Transport failures carry ctx.Canceled / context.DeadlineExceeded
		// as their cause; keep the cause for errors.Is while the message
		// stays generic and leaks nothing from the wire.
		return httpResult{}, networkError(
			"the registry request failed",
			"check the network connection and try again",
			err,
		)
	}
	defer func() { _ = httpResp.Body.Close() }() //nolint:errcheck // best-effort close of a drained response body

	body, err := readCapped(httpResp.Body, maxBytes)
	if err != nil {
		return httpResult{}, err
	}

	if httpResp.StatusCode == http.StatusUnauthorized {
		return httpResult{
			body:      body,
			status:    httpResp.StatusCode,
			challenge: httpResp.Header.Get("WWW-Authenticate"),
		}, nil
	}

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return httpResult{}, statusError("registry request", httpResp.StatusCode)
	}

	return httpResult{
		body:          body,
		status:        httpResp.StatusCode,
		contentType:   httpResp.Header.Get("Content-Type"),
		contentDigest: httpResp.Header.Get(dockerContentDigestHeader),
	}, nil
}

// tokenResponse is the v2 token-endpoint JSON shape. Registries return the
// anonymous bearer token under "token" (the spec field) or "access_token"
// (the OAuth2 alias); both are accepted.
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

// fetchAnonymousToken performs the anonymous Bearer token request implied by
// a 401 WWW-Authenticate challenge. It parses the realm, service, and scope
// from the challenge, builds realm?service=...&scope=..., and GETs it WITHOUT
// any credentials. Any failure — unparseable challenge, transport
// error, non-2xx token status, decode failure, or empty token — maps to a
// network failure. The returned token is never logged.
func (c *Client) fetchAnonymousToken(ctx context.Context, ref Reference, challenge string) (string, error) {
	params, ok := parseBearerChallenge(challenge)
	if !ok {
		return "", networkError(
			"the registry authentication challenge is unsupported",
			"this client supports anonymous Bearer authentication only",
			nil,
		)
	}

	realm := params["realm"]
	if realm == "" {
		return "", networkError("the registry authentication challenge has no realm", "", nil)
	}

	tokenURL, err := url.Parse(realm)
	if err != nil || (tokenURL.Scheme != "http" && tokenURL.Scheme != "https") {
		return "", networkError("the registry token endpoint is invalid", "", err)
	}

	query := tokenURL.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	// Prefer the challenge's scope; fall back to the standard pull scope
	// for the target repository so registries that omit scope still grant
	// a read token.
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + ref.Repository + ":pull"
	}
	query.Set("scope", scope)
	tokenURL.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", networkError("building the registry token request failed", "", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.doer.Do(req)
	if err != nil {
		return "", networkError(
			"the registry token request failed",
			"check the network connection and try again",
			err,
		)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close of a drained response body

	body, err := readCapped(resp.Body, maxTokenBytes)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", statusError("registry token request", resp.StatusCode)
	}

	var decoded tokenResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", networkError(
			"parsing the registry token response failed",
			"the registry returned an unexpected token response shape",
			err,
		)
	}

	token := decoded.Token
	if token == "" {
		token = decoded.AccessToken
	}
	if token == "" {
		return "", networkError("the registry returned an empty token", "", nil)
	}

	return token, nil
}

// manifestURL builds the v2 manifest endpoint for a reference. The repository
// and tag have already passed [ParseReference] validation, so they are safe
// path components, but each is still escaped defensively.
func (c *Client) manifestURL(ref Reference) string {
	return fmt.Sprintf("%s://%s/v2/%s/manifests/%s",
		c.scheme, ref.Registry, escapeRepositoryPath(ref.Repository), url.PathEscape(ref.Tag))
}

// escapeRepositoryPath path-escapes each repository component while
// preserving the "/" separators, so a validated "library/nginx" stays
// "library/nginx" in the URL rather than collapsing to "library%2Fnginx".
func escapeRepositoryPath(repository string) string {
	parts := strings.Split(repository, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// readCapped reads from r up to maxBytes, reading one byte past the cap so an
// exactly-at-cap body still reads fully while an over-cap body stays
// detectable, and maps an over-cap or read failure to a network error.
func readCapped(r io.Reader, maxBytes int64) ([]byte, error) {
	limited := io.LimitReader(r, maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, networkError("reading the registry response failed", "", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, networkError(
			"the registry response exceeds the size limit",
			"the registry returned more data than allowed and was refused",
			errBodyTooLarge,
		)
	}
	return body, nil
}

// statusError maps a non-2xx HTTP status to a typed network failure.
// Registries signal rate limiting with 429 (and some with 403); both get a
// "rate limit" message and a wait-and-retry hint. Every other non-2xx status
// names only the operation kind and the numeric code — never the response
// body — so nothing from the wire leaks into the surfaced error.
func statusError(kind string, status int) error {
	if status == http.StatusTooManyRequests || status == http.StatusForbidden {
		return networkError(
			fmt.Sprintf("the registry rate limit was reached on the %s", kind),
			"wait for the registry rate limit to reset and try again",
			nil,
		)
	}
	return networkError(
		fmt.Sprintf("the %s returned http status %d", kind, status),
		"",
		nil,
	)
}

// errBodyTooLarge is the cause attached when a response exceeds its size
// cap, so callers can branch on the size-cap failure via errors.Is while
// the surfaced *Error stays a generic network failure.
var errBodyTooLarge = errors.New("registry: response exceeds size cap")

// networkError wraps a fail-closed network/transport fault with the
// [types.ErrCodeNetworkFailure] exit code. It is the
// single error constructor for every transport, status, token, size-cap,
// decode, and cancellation failure, so the whole client maps to one exit code
// regardless of which step rejected. It never produces a
// [types.ErrCodeVerificationFailed]; this client does no trust verification.
func networkError(message, hint string, cause error) error {
	if cause != nil {
		return types.WrapError(types.ErrCodeNetworkFailure, message, hint, cause)
	}
	return types.NewError(types.ErrCodeNetworkFailure, message, hint)
}

// usageError wraps a malformed-caller-input fault with the
// [types.ErrCodeUsageValidation] exit code. A bad
// image reference is the caller's mistake, distinct from a network failure,
// per the "Network and trust failures are distinct" convention.
func usageError(message, hint string) error {
	return types.NewError(types.ErrCodeUsageValidation, message, hint)
}
