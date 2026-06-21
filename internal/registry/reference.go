package registry

import (
	"strings"
)

// Reference is a parsed, validated image reference split into the registry
// host, the repository path, and the tag. It is the only shape the client
// accepts — callers go through [ParseReference], which normalizes Docker Hub
// shorthand (e.g. "nginx" -> registry-1.docker.io / library/nginx / latest)
// and rejects malformed input fail-closed. A Reference carries no network
// state.
type Reference struct {
	// Registry is the registry host (and optional port), e.g.
	// "registry-1.docker.io", "ghcr.io", or "localhost:5000". It is the
	// host the v2 API calls target.
	Registry string

	// Repository is the repository path WITHOUT the registry host, e.g.
	// "library/nginx" or "louislam/uptime-kuma". Docker Hub single-name
	// images are normalized into the "library/" namespace.
	Repository string

	// Tag is the requested tag, defaulting to "latest" when the input omits
	// one. ParseReference rejects digest-pinned references — this client
	// resolves a tag TO a digest, so a caller pinning a digest has nothing
	// to resolve.
	Tag string
}

const (
	// defaultRegistry is the registry Docker Hub shorthand resolves to. It
	// is the v2 API host (registry-1.docker.io), NOT the docker.io web
	// alias, because the metadata API lives on registry-1.
	defaultRegistry = "registry-1.docker.io"

	// defaultNamespace is the implicit namespace Docker Hub single-name
	// images live under, e.g. "nginx" -> "library/nginx".
	defaultNamespace = "library"

	// defaultTag is the tag assumed when an image reference omits one.
	defaultTag = "latest"

	// localhostHost is the one bare (dotless, colonless) host string that
	// is still treated as a registry rather than a repository namespace,
	// matching Docker's reference grammar.
	localhostHost = "localhost"

	// maxReferenceLen bounds the accepted reference length so a hostile
	// caller cannot push a multi-megabyte string through the parser and the
	// validators. Real references are well under 256 bytes.
	maxReferenceLen = 4096
)

// ParseReference parses and validates an image reference of the form
// "[registry[:port]/]repository[:tag]" into a normalized [Reference].
// Normalization mirrors Docker's reference grammar: the first slash
// component is the registry only when it looks like a host (contains "."
// or ":", or equals "localhost"); otherwise the whole path is a Docker Hub
// repository and the default registry applies. A single-segment Docker Hub
// repository gains the "library/" namespace, and a missing tag defaults to
// "latest".
// It returns a typed [types.ErrCodeUsageValidation] error (exit 2) for any
// malformed input — a blank reference, an over-length reference, a
// digest-pinned reference (nothing to resolve), an empty tag after a colon,
// a tag with illegal characters, an empty repository, or a repository path
// component that violates the registry path grammar. Validation happens
// before any network use, so bad caller input never reaches the wire.
func ParseReference(ref string) (Reference, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return Reference{}, usageError("image reference is empty", "provide an image reference like registry/repository:tag")
	}
	if len(trimmed) > maxReferenceLen {
		return Reference{}, usageError("image reference is too long", "provide a shorter image reference")
	}

	// A digest pin ("repo@sha256:...") has nothing to resolve — this client
	// turns a tag INTO a digest. Refuse it as caller error rather than
	// silently dropping the digest.
	if strings.Contains(trimmed, "@") {
		return Reference{}, usageError(
			"digest-pinned image references are not supported",
			"provide a tag reference like registry/repository:tag, not an @digest pin",
		)
	}

	registry, remainder := splitRegistry(trimmed)

	repository, tag, err := splitRepositoryTag(remainder)
	if err != nil {
		return Reference{}, err
	}

	if registry == defaultRegistry && !strings.Contains(repository, "/") {
		repository = defaultNamespace + "/" + repository
	}

	if err := validateRepository(repository); err != nil {
		return Reference{}, err
	}
	if err := validateTag(tag); err != nil {
		return Reference{}, err
	}

	return Reference{Registry: registry, Repository: repository, Tag: tag}, nil
}

// splitRegistry separates the registry host from the rest of the reference.
// The first slash-delimited component is the registry only when it is
// host-shaped (contains ".", equals "localhost", or is a "host:port" with a
// digit port); otherwise the reference is a Docker Hub image and the default
// registry applies. The digit-port rule disambiguates a colon in the first
// segment so a malformed name like "nginx:bad/tag" is NOT mistaken for a
// registry host and falls through to repository validation, which rejects it.
func splitRegistry(ref string) (registry, remainder string) {
	first, rest, found := strings.Cut(ref, "/")
	if !found {
		return defaultRegistry, ref
	}
	if isRegistryHost(first) {
		return first, rest
	}
	return defaultRegistry, ref
}

// isRegistryHost reports whether the first reference segment is a registry
// host rather than a repository-namespace segment, matching Docker's
// reference grammar: a host contains a ".", equals "localhost", or carries
// a numeric ":port" suffix.
func isRegistryHost(segment string) bool {
	if strings.Contains(segment, ".") || segment == localhostHost {
		return true
	}
	host, port, found := strings.Cut(segment, ":")
	if !found || host == "" || port == "" {
		return false
	}
	for i := range len(port) {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	return true
}

// splitRepositoryTag splits the registry-stripped remainder into its
// repository and tag. The tag is the substring after the LAST colon, taken
// only when that colon follows the last slash, so a leftover "host:port"
// (which splitRegistry should already have removed, but is guarded
// defensively) or a port-shaped segment is not mistaken for a tag. A trailing
// colon with no tag, or an empty repository, is a usage error.
func splitRepositoryTag(remainder string) (repository, tag string, err error) {
	lastSlash := strings.LastIndex(remainder, "/")
	lastColon := strings.LastIndex(remainder, ":")

	if lastColon > lastSlash {
		repository = remainder[:lastColon]
		tag = remainder[lastColon+1:]
		if tag == "" {
			return "", "", usageError("image reference has an empty tag", "provide a tag after the colon, e.g. repository:1.2.3")
		}
	} else {
		repository = remainder
		tag = defaultTag
	}

	if repository == "" {
		return "", "", usageError("image reference has an empty repository", "provide a repository like registry/repository:tag")
	}

	return repository, tag, nil
}

// validateRepository enforces the OCI / Docker repository path grammar on
// the registry-stripped repository: one or more lowercase path components
// separated by "/", each a run of alphanumerics with internal ".", "_",
// "__", or "-" separators. It rejects empty components, leading or trailing
// separators, and any character outside the grammar, so a hostile path cannot
// smuggle a traversal or a query string into the v2 URL.
func validateRepository(repository string) error {
	if repository == "" {
		return usageError("image reference has an empty repository", "provide a repository like registry/repository:tag")
	}

	for component := range strings.SplitSeq(repository, "/") {
		if !validRepositoryComponent(component) {
			return usageError(
				"image reference has an invalid repository path",
				"use lowercase letters, digits, and separators (._-) in each path segment",
			)
		}
	}

	return nil
}

// validRepositoryComponent reports whether one slash-delimited repository
// segment matches the OCI path-component grammar: an alphanumeric run,
// optionally followed by groups of a single separator (".", "-", "_", or
// "__") and another alphanumeric run. Separators are always internal and
// never doubled except the explicit "__".
func validRepositoryComponent(component string) bool {
	if component == "" {
		return false
	}

	i := 0
	// Leading alphanumeric run is mandatory.
	start := i
	for i < len(component) && isAlphanumericLower(component[i]) {
		i++
	}
	if i == start {
		return false
	}

	for i < len(component) {
		sepStart := i
		// Consume a separator group: ".", "_", "__", or "-".
		switch component[i] {
		case '.', '-':
			i++
		case '_':
			i++
			if i < len(component) && component[i] == '_' {
				i++
			}
		default:
			return false
		}
		if i == sepStart {
			return false
		}

		// A separator must be followed by an alphanumeric run.
		runStart := i
		for i < len(component) && isAlphanumericLower(component[i]) {
			i++
		}
		if i == runStart {
			return false
		}
	}

	return true
}

// validateTag enforces the OCI tag grammar: 1-128 characters from
// [A-Za-z0-9_.-], with a first character that is not "." or "-". It rejects
// slashes, whitespace, and control bytes so a tag cannot break out of the
// manifest URL path.
func validateTag(tag string) error {
	if tag == "" || len(tag) > 128 {
		return usageError("image reference has an invalid tag", "use a tag of 1-128 characters from [A-Za-z0-9_.-]")
	}
	if tag[0] == '.' || tag[0] == '-' {
		return usageError("image reference has an invalid tag", "a tag may not start with a dot or dash")
	}
	for i := range len(tag) {
		if !isTagByte(tag[i]) {
			return usageError("image reference has an invalid tag", "use a tag of 1-128 characters from [A-Za-z0-9_.-]")
		}
	}
	return nil
}

// isAlphanumericLower reports whether b is a lowercase letter or a digit.
// Repository components are lowercase-only per the OCI grammar.
func isAlphanumericLower(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// isTagByte reports whether b is a byte permitted in an OCI tag.
func isTagByte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z':
		return true
	case b >= 'a' && b <= 'z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '_' || b == '.' || b == '-':
		return true
	default:
		return false
	}
}
