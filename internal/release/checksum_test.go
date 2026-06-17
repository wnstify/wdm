package release_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/release"
	"github.com/wnstify/wdm/pkg/types"
)

// sumLine builds a GNU coreutils "<hex> <name>" line (two-space text-mode
// separator) for the SHA-256 of body.
func sumLine(name string, body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]) + "  " + name
}

func TestParseSHA256SUMS_Valid(t *testing.T) {
	t.Parallel()

	bin := []byte("the wdm binary bytes")
	cat := []byte("the catalog bundle bytes")

	sums := strings.Join([]string{
		"# a comment line is ignored",
		"",
		sumLine("wdm-linux-amd64", bin),
		sumLine("catalog-stable.tar.gz", cat),
	}, "\n")

	parsed, err := release.ParseSHA256SUMS([]byte(sums))
	require.NoError(t, err)
	require.Len(t, parsed, 2)

	binSum := sha256.Sum256(bin)
	assert.Equal(t, hex.EncodeToString(binSum[:]), parsed["wdm-linux-amd64"])
}

func TestParseSHA256SUMS_ToleratesBinaryMarkerAndSeparators(t *testing.T) {
	t.Parallel()

	bin := []byte("binary mode artifact")
	binSum := hex.EncodeToString(sha256Of(bin))

	// Single-space "*" binary marker form: "<hex> *<name>".
	sums := binSum + " *wdm-linux-amd64\n"

	parsed, err := release.ParseSHA256SUMS([]byte(sums))
	require.NoError(t, err)
	assert.Equal(t, binSum, parsed["wdm-linux-amd64"])
}

func TestParseSHA256SUMS_FailsClosed(t *testing.T) {
	t.Parallel()

	validHex := hex.EncodeToString(sha256Of([]byte("x")))

	tests := []struct {
		name  string
		input string
	}{
		{name: "empty input", input: ""},
		{name: "whitespace only", input: "   \n\t\n"},
		{name: "comments only", input: "# just a comment\n# another\n"},
		{name: "short digest", input: "deadbeef  wdm-linux-amd64\n"},
		{name: "non hex digest", input: strings.Repeat("z", 64) + "  name\n"},
		{name: "missing name", input: validHex + "  \n"},
		{name: "missing separator", input: validHex + "\n"},
		{
			name:  "duplicate name",
			input: validHex + "  dup\n" + validHex + "  dup\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parsed, err := release.ParseSHA256SUMS([]byte(tt.input))
			assert.Nil(t, parsed)
			require.Error(t, err)
			assert.True(
				t,
				types.IsCode(err, types.ErrCodeVerificationFailed),
				"want ErrCodeVerificationFailed, got %v", err,
			)
		})
	}
}

func TestVerifyChecksum_Valid(t *testing.T) {
	t.Parallel()

	bin := []byte("verify me exactly")
	sums, err := release.ParseSHA256SUMS([]byte(sumLine("wdm-linux-amd64", bin)))
	require.NoError(t, err)

	assert.NoError(t, release.VerifyChecksum(sums, "wdm-linux-amd64", bin))
}

func TestVerifyChecksum_FailsClosed(t *testing.T) {
	t.Parallel()

	bin := []byte("the real artifact")
	good := release.HexDigest(bin)

	sums := map[string]string{"wdm-linux-amd64": good}

	t.Run("digest mismatch", func(t *testing.T) {
		t.Parallel()

		err := release.VerifyChecksum(sums, "wdm-linux-amd64", []byte("tampered artifact"))
		require.Error(t, err)
		assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed))
	})

	t.Run("name absent", func(t *testing.T) {
		t.Parallel()

		err := release.VerifyChecksum(sums, "not-in-sums", bin)
		require.Error(t, err)
		assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed))
	})

	t.Run("empty artifact", func(t *testing.T) {
		t.Parallel()

		err := release.VerifyChecksum(sums, "wdm-linux-amd64", nil)
		require.Error(t, err)
		assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed))
	})

	t.Run("digest field not hex in map", func(t *testing.T) {
		t.Parallel()

		bad := map[string]string{"wdm-linux-amd64": "not-hex"}
		err := release.VerifyChecksum(bad, "wdm-linux-amd64", bin)
		require.Error(t, err)
		assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed))
	})
}

// sha256Of is a small local helper returning the raw SHA-256 of body.
func sha256Of(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}

// Cross-check: HexDigest agrees with crypto/sha256 directly.
func TestHexDigest_MatchesStdlib(t *testing.T) {
	t.Parallel()

	body := []byte("digest me")
	want := hex.EncodeToString(sha256Of(body))
	assert.Equal(t, want, release.HexDigest(body))
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(body)), release.HexDigest(body))
}
