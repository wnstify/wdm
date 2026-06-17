package registry

import (
	"encoding/json"
	"strings"
)

// mediaTypeFromContentType extracts the bare media type from a Content-Type
// header value, dropping any parameters after a ";" (e.g. a charset) and
// surrounding whitespace. An empty header yields an empty string, letting the
// caller fall back to the body's declared mediaType.
func mediaTypeFromContentType(contentType string) string {
	base, _, _ := strings.Cut(contentType, ";")
	return strings.TrimSpace(base)
}

// knownManifestMediaTypes is the set of OCI / Docker manifest and index
// media types the client recognizes as authoritative when set as a
// response Content-Type.
var knownManifestMediaTypes = map[string]struct{}{
	"application/vnd.oci.image.manifest.v1+json":                {},
	"application/vnd.oci.image.index.v1+json":                   {},
	"application/vnd.docker.distribution.manifest.v2+json":      {},
	"application/vnd.docker.distribution.manifest.list.v2+json": {},
}

// isManifestMediaType reports whether mediaType is one of the recognized
// OCI / Docker manifest or index media types. A generic or sniffed type
// (e.g. "text/plain" or "application/json") returns false, so the caller
// falls back to the body's declared mediaType.
func isManifestMediaType(mediaType string) bool {
	_, ok := knownManifestMediaTypes[mediaType]
	return ok
}

// manifestMediaTypeEnvelope is the minimal projection used to recover the
// manifest media type from the body when the registry omits or mis-sets the
// Content-Type header. Both OCI and Docker v2 manifests and indexes carry a
// top-level "mediaType" field.
type manifestMediaTypeEnvelope struct {
	MediaType string `json:"mediaType"`
}

// mediaTypeFromBody recovers the manifest media type from the manifest JSON
// body's top-level "mediaType" field. It is a best-effort fallback for
// registries that do not set an accurate Content-Type; a parse failure or
// missing field yields an empty string rather than an error, because the
// media type is advisory metadata and the digest (the value callers act on)
// is computed independently.
func mediaTypeFromBody(body []byte) string {
	var envelope manifestMediaTypeEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return strings.TrimSpace(envelope.MediaType)
}
