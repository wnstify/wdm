package release

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"github.com/wnstify/wdm/pkg/types"
)

// sha256HexLen is the length of a hex-encoded SHA-256 digest (32 bytes ->
// 64 hex characters). A SHA256SUMS line whose digest field is not exactly
// this length is malformed and rejected.
const sha256HexLen = 2 * sha256.Size

// ParseSHA256SUMS parses GNU coreutils `sha256sum` output into a map of
// artifact name -> lowercase hex SHA-256 digest. Each line is
// "<64-hex> <name>": two ASCII spaces separate the fields in text mode,
// and the single-space "*" binary marker before the name is also
// tolerated ("<64-hex> *<name>"). Blank and "#"-prefixed comment lines
// are skipped.
// It fails closed (PRD §22, §23): empty input, a malformed digest field,
// a missing name, a missing separator, or a duplicate name return a typed
// [types.ErrCodeVerificationFailed] error rather than a partial map.
// Parsing is strict because SHA256SUMS is the integrity anchor every
// other artifact is checked against; a silently dropped or mis-parsed
// line would weaken that anchor.
func ParseSHA256SUMS(sums []byte) (map[string]string, error) {
	if len(bytes.TrimSpace(sums)) == 0 {
		return nil, verificationError("checksum file is empty", "", nil)
	}

	digests := map[string]string{}

	scanner := bufio.NewScanner(bytes.NewReader(sums))
	// Allow long lines defensively; names are short in practice, and the
	// cap bounds memory.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++

		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, digest, err := parseSumLine(line)
		if err != nil {
			return nil, err
		}

		if _, dup := digests[name]; dup {
			return nil, verificationError(
				"checksum file lists a name more than once",
				"each artifact must appear exactly once in SHA256SUMS",
				nil,
			)
		}

		digests[name] = digest
	}

	if err := scanner.Err(); err != nil {
		return nil, verificationError("reading checksum file failed", "", err)
	}

	if len(digests) == 0 {
		return nil, verificationError("checksum file has no entries", "", nil)
	}

	return digests, nil
}

// parseSumLine splits one non-comment, non-blank SHA256SUMS line into its
// lowercased hex digest and artifact name, checking the digest field is a
// well-formed 64-character lowercase hex string.
func parseSumLine(line string) (name, digest string, err error) {
	// The digest is the first whitespace-delimited field; everything after
	// the separator is the name (which may, defensively, contain spaces).
	digestField, rest, found := strings.Cut(line, " ")
	if !found || digestField == "" {
		return "", "", verificationError(
			"malformed line in checksum file",
			"expected '<sha256-hex>  <name>' lines",
			nil,
		)
	}

	// Tolerate the coreutils two-space text separator and the single-space
	// "*" binary marker: trim leading spaces and one optional "*" before
	// the name.
	rest = strings.TrimLeft(rest, " ")
	rest = strings.TrimPrefix(rest, "*")

	name = rest
	if name == "" {
		return "", "", verificationError(
			"checksum file line is missing a name",
			"expected '<sha256-hex>  <name>' lines",
			nil,
		)
	}

	digest = strings.ToLower(digestField)
	if !isHexSHA256(digest) {
		return "", "", verificationError(
			"malformed digest in checksum file",
			"expected a 64-character hex sha256 digest",
			nil,
		)
	}

	return name, digest, nil
}

// VerifyChecksum confirms that artifact hashes to the SHA-256 digest the
// parsed SHA256SUMS map records for name. The map comes from
// [ParseSHA256SUMS]; name is the entry the artifact bytes must match.
// It fails closed (PRD §22, §23): empty artifact bytes, a name absent from
// the sums map, or a digest mismatch return a typed
// [types.ErrCodeVerificationFailed] error. The comparison is constant-time
// ([crypto/subtle]) so a timing side channel cannot leak how much of an
// attacker-supplied artifact matched.
func VerifyChecksum(sums map[string]string, name string, artifact []byte) error {
	if len(artifact) == 0 {
		return verificationError("artifact is empty", "", nil)
	}

	expectedHex, ok := sums[name]
	if !ok {
		return verificationError(
			"artifact is not listed in the checksum file",
			"the release SHA256SUMS does not cover this artifact name",
			nil,
		)
	}

	expected, err := hex.DecodeString(expectedHex)
	if err != nil {
		// ParseSHA256SUMS validates the hex shape, so this only triggers
		// for a hand-built map; treat it as a verification failure rather
		// than trust an unparseable expectation.
		return verificationError("checksum file digest is not valid hex", "", err)
	}

	actual := sha256.Sum256(artifact)
	if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
		return verificationError(
			"artifact checksum does not match the release checksum",
			"the downloaded artifact is corrupt or has been tampered with",
			nil,
		)
	}

	return nil
}

// isHexSHA256 reports whether s is exactly 64 lowercase hex characters.
func isHexSHA256(s string) bool {
	if len(s) != sha256HexLen {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// verificationError wraps a fail-closed verification fault with the
// [types.ErrCodeVerificationFailed] exit code. It
// is the single error constructor for every primitive in this package, so
// trust failures map to one exit code regardless of which check rejected.
func verificationError(message, hint string, cause error) error {
	if cause != nil {
		return types.WrapError(types.ErrCodeVerificationFailed, message, hint, cause)
	}
	return types.NewError(types.ErrCodeVerificationFailed, message, hint)
}
