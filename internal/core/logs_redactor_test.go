package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/security"
)

// The Logs path seeds its redactor from the stack .env via validateConfigRedactor
// so a bare secret literal echoed into a log line is scrubbed. A stack without a
// .env still yields a working (structural-only) redactor rather than failing.
func TestValidateConfigRedactor_SeedsFromStackEnv(t *testing.T) {
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
