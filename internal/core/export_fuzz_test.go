package core

// NormalizeDomainForTest exposes the unexported normalizeDomain so the
// domain-validator fuzz target in package core_test can drive the real
// RFC-1123 normalization surface. It is a thin, behavior-preserving seam: the
// returned host and error are normalizeDomain's verbatim outputs.
func NormalizeDomainForTest(value string) (string, error) {
	return normalizeDomain(value)
}
