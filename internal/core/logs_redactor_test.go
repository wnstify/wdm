package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/security"
)

// WDM-SEC-006: the Logs path must seed its redactor from the stack .env VALUES
// (via the real validateConfigRedactor seam) so a bare generated-secret literal
// echoed into a container log line is scrubbed; the prior nil-redactor path left
// it in cleartext. This drives the real validateConfigRedactor and asserts the
// literal is replaced with the redaction placeholder (no mock).
func TestValidateConfigRedactor_WDMSEC006_SeedsFromStackEnv(t *testing.T) {
	t.Parallel()

	stackPath := t.TempDir()
	envBody := "SERPBEAR_PASSWORD=s3cr3t-literal\nOTHER=value\n"
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, ".env"), []byte(envBody), 0o600))

	redactor, err := validateConfigRedactor(stackPath)
	require.NoError(t, err)

	got := redactor.Redact("login as admin with s3cr3t-literal now")
	require.NotContains(t, got, "s3cr3t-literal")
	require.Contains(t, got, security.RedactedPlaceholder)
}

func TestValidateConfigRedactor_NoEnvStillBuildsRedactor(t *testing.T) {
	t.Parallel()

	redactor, err := validateConfigRedactor(t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, redactor)
	// No literals to scrub; structural-only redaction passes plain text through.
	require.Equal(t, "plain line", redactor.Redact("plain line"))
}
