package registry

import (
	"strings"
)

// parseBearerChallenge parses a WWW-Authenticate header value of the form
//
//	Bearer realm="https://auth.example/token",service="reg.example",scope="repository:lib/x:pull"
//
// into its parameter map and reports whether the scheme is Bearer. It is the
// anonymous-token-dance entry point: the realm, service, and scope it
// extracts drive [Client.fetchAnonymousToken]. A non-Bearer scheme (e.g.
// Basic) returns ok=false so the caller fails closed — this client does not
// handle credential-based authentication.
// Parsing tolerates optional whitespace, quoted or unquoted values, and a
// trailing comma. It never executes anything from the header; the values
// serve only as URL query parameters, which url.Values escapes.
func parseBearerChallenge(challenge string) (params map[string]string, ok bool) {
	trimmed := strings.TrimSpace(challenge)

	// The scheme token is the first space-delimited word and must be
	// "Bearer" (case-insensitive per RFC 7235).
	scheme, rest, found := strings.Cut(trimmed, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}

	params = make(map[string]string)
	for _, part := range splitChallengeParams(rest) {
		key, value, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if key != "" {
			params[strings.ToLower(key)] = value
		}
	}

	return params, true
}

// splitChallengeParams splits the comma-separated parameter list of a Bearer
// challenge, respecting double-quoted values so a comma inside a quoted scope
// (which the registry spec permits) does not split the parameter. It returns
// the raw key="value" or key=value fragments.
func splitChallengeParams(rest string) []string {
	var (
		parts    []string
		current  strings.Builder
		inQuotes bool
	)

	for i := range len(rest) {
		ch := rest[i]
		switch {
		case ch == '"':
			inQuotes = !inQuotes
			current.WriteByte(ch)
		case ch == ',' && !inQuotes:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteByte(ch)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	return parts
}
