package core_test

import (
	"bytes"
	"log/slog"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/logging"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/pkg/types"
)

// TestInstall_RedactorScrubsGeneratedSecretsFromLogs is the issue #119 prefactor
// safety net: it proves no generated secret plaintext ever survives in log
// output, exercising the REAL secret-generation and REAL render/redactor path —
// never a mock of the protected seam.
//
// It drives a real plan+render via [core.RenderInstallForTest] so the engine
// mints two real secrets (one base64url, one hex) and renders the stack in
// memory, then binds those exact rendered values into the production redactor
// ([security.NewActiveRedactor]) behind the production slog redaction handler
// ([logging.NewRedactingHandler]) over a captured buffer. Log lines that echo
// each freshly minted secret — and the full rendered .env bytes — must come out
// scrubbed. A regression that leaks a secret into a log call, or fails to bind a
// generated value to the redactor, fails this test.
func TestInstall_RedactorScrubsGeneratedSecretsFromLogs(t *testing.T) {
	t.Parallel()

	const (
		base64Secret = "render-canary-base64url-secret-value"
		hexSecret    = "abcdef0123456789feedface"
	)

	app := appFixture("redactor-leak-app", freeLocalTCPPort(t))
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/redactor-leak/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/redactor-leak/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
		{Name: "API_TOKEN", Type: "secret", Required: true, Encoding: "hex"},
	}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/redactor-leak/docker-compose.yml.tmpl": "services:\n" +
			"  app:\n" +
			"    image: docker.io/example/app:1.0.0\n" +
			"    environment:\n" +
			"      DB_PASSWORD: ${DB_PASSWORD}\n" +
			"      API_TOKEN: ${API_TOKEN}\n",
		"templates/redactor-leak/.env.tmpl": "DB_PASSWORD={{ .DB_PASSWORD }}\nAPI_TOKEN={{ .API_TOKEN }}\n",
	}, app)

	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, func(enc security.Encoding) (string, error) {
		switch enc {
		case security.EncodingBase64URL:
			return base64Secret, nil
		case security.EncodingHex:
			return hexSecret, nil
		default:
			return "", assert.AnError
		}
	})

	// REAL plan + render: mints the real secrets and renders the stack.
	snapshot, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"DB_PASSWORD", "API_TOKEN"}, snapshot.GeneratedFields)

	// Collect the real generated values straight off the rendered plan, then
	// bind them into the production redactor + slog redaction handler exactly as
	// install wires its log sink.
	secrets := make([]string, 0, len(snapshot.GeneratedFields))
	for _, field := range snapshot.GeneratedFields {
		value := snapshot.ResolvedValues[field]
		require.NotEmpty(t, value, "generated field %q must resolve to a value", field)
		secrets = append(secrets, value)
	}
	require.ElementsMatch(t, []string{base64Secret, hexSecret}, secrets)

	var buf bytes.Buffer
	logger := slog.New(logging.NewRedactingHandler(
		slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
		security.NewActiveRedactor(secrets),
	))

	// Drive log lines that echo each minted secret plus the full rendered .env
	// bytes (which carry both secrets) through the real redaction pipeline.
	for i, value := range secrets {
		logger.Info("install: echoing generated secret", slog.String("field_"+strconv.Itoa(i), value))
	}
	logger.Info("install: rendered env", slog.String("env", string(snapshot.EnvBytes)))

	out := buf.String()
	require.NotEmpty(t, out)
	assert.NotContains(t, out, base64Secret, "base64url generated secret leaked into log output")
	assert.NotContains(t, out, hexSecret, "hex generated secret leaked into log output")
	assert.Contains(t, out, security.RedactedPlaceholder, "scrubbed secrets must surface as the redaction placeholder")
}
