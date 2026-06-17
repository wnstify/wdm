package docker

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

func TestNew_ReturnsProductionExecClient(t *testing.T) {
	t.Parallel()

	client, err := New()
	require.NoError(t, err)
	require.NotNil(t, client)

	impl, ok := client.(*execClient)
	require.True(t, ok, "New must return *execClient behind Client")
	require.NotNil(t, impl.redactor)
	require.NotNil(t, impl.execFn)
}

func TestNew_WithRedactorInjectsDependency(t *testing.T) {
	t.Parallel()

	redactor := security.NewActiveRedactor([]string{"top-secret"})
	client, err := New(WithRedactor(redactor))
	require.NoError(t, err)

	impl, ok := client.(*execClient)
	require.True(t, ok)
	require.Same(t, redactor, impl.redactor)
}

func TestNew_WithCommandExecutorInjectsDependency(t *testing.T) {
	t.Parallel()

	want := CommandResult{
		Stdout: "ok",
		Stderr: "redacted",
	}
	invoked := false
	execFn := func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		invoked = true
		require.Equal(t, []string{"compose", "version"}, cmd.argv)
		return want, nil
	}

	client, err := New(WithCommandExecutor(execFn))
	require.NoError(t, err)

	got, err := client.Run(
		t.Context(),
		ComposeVersionInvocation{},
	)
	require.NoError(t, err)
	require.True(t, invoked, "injected executor must be called")
	require.Equal(t, want, got)
}

func TestRun_UsesTypedShapeForComposeDownWithoutDashV(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	invoked := false
	execFn := func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		invoked = true
		require.Equal(
			t,
			[]string{
				"compose",
				"-f",
				project.ComposeFile,
				"--env-file",
				project.EnvFile,
				"--project-name",
				project.ProjectName,
				"down",
			},
			cmd.argv,
		)
		require.NotContains(t, cmd.argv, "-v")
		return CommandResult{}, nil
	}

	client, err := New(WithCommandExecutor(execFn))
	require.NoError(t, err)

	_, err = client.Run(
		t.Context(),
		composeDownInvocation{
			composeFile: project.ComposeFile,
			envFile:     project.EnvFile,
			projectName: project.ProjectName,
		},
	)
	require.NoError(t, err)
	require.True(t, invoked, "typed compose-down invocation must be executable")
}

func TestRun_RedactsStderrFromExecutor(t *testing.T) {
	t.Parallel()

	execFn := func(_ context.Context, _ commandSpec) (CommandResult, error) {
		return CommandResult{
			Stderr: "leaked: top-secret",
		}, nil
	}

	client, err := New(
		WithRedactor(security.NewActiveRedactor([]string{"top-secret"})),
		WithCommandExecutor(execFn),
	)
	require.NoError(t, err)

	got, err := client.Run(t.Context(), ComposeVersionInvocation{})
	require.NoError(t, err)
	require.Equal(t, "leaked: "+security.RedactedPlaceholder, got.Stderr)
}

func TestNew_RejectsNilRedactor(t *testing.T) {
	t.Parallel()

	client, err := New(WithRedactor(nil))
	require.Nil(t, client)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNilRedactor)
}

func TestNew_RejectsNilCommandExecutor(t *testing.T) {
	t.Parallel()

	client, err := New(WithCommandExecutor(nil))
	require.Nil(t, client)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNilCommandExecutor)
}

func TestRun_CanceledContextBeforeExecutionReturnsTypedUserCanceled(t *testing.T) {
	t.Parallel()

	client, err := New()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = client.Run(ctx, VersionInvocation{})
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUserCanceled, typedErr.Code)
}

func TestNew_WrapsOptionError(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	client, err := New(func(*config) error {
		return boom
	})
	require.Nil(t, client)
	require.Error(t, err)
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "docker.New:")
}

func TestRun_RejectsNilInvocation(t *testing.T) {
	t.Parallel()

	client, err := New()
	require.NoError(t, err)

	_, err = client.Run(t.Context(), nil)
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
}

func TestRun_DefaultExecutorRunsDockerArgvAndRedactsStreams(t *testing.T) {
	fakeDocker := `#!/bin/sh
printf 'argv='
for arg in "$@"; do
  printf '[%s]' "$arg"
done
printf '\n'
printf 'out=%s\n' "$WDM_SECRET"
printf 'err=%s\n' "$WDM_SECRET" >&2
`

	secret := "top-secret"
	useFakeDocker(t, fakeDocker)
	t.Setenv("WDM_SECRET", secret)

	client, err := New(WithRedactor(security.NewActiveRedactor([]string{secret})))
	require.NoError(t, err)

	got, err := client.Run(t.Context(), ComposeVersionInvocation{})
	require.NoError(t, err)
	require.Contains(t, got.Stdout, "argv=[compose][version]")
	require.Contains(t, got.Stdout, "out="+security.RedactedPlaceholder)
	require.Contains(t, got.Stderr, "err="+security.RedactedPlaceholder)
	require.NotContains(t, got.Stdout, secret)
	require.NotContains(t, got.Stderr, secret)
}

func TestBuildDockerExecCmd_ConfiguresWaitDelayAndCancelHook(t *testing.T) {
	t.Parallel()

	cmd := buildDockerExecCmd(t.Context(), []string{"version"})
	require.NotNil(t, cmd)
	require.Equal(t, dockerCancelWaitDelay, cmd.WaitDelay)
	require.Equal(t, 30*time.Second, cmd.WaitDelay)
	require.NotNil(t, cmd.Cancel)
	require.NoError(t, cmd.Cancel())
}

func TestRun_DefaultExecutorContextCancellationUsesSIGTERM(t *testing.T) {
	termFile := filepath.Join(t.TempDir(), "term.signal")
	startedFile := filepath.Join(t.TempDir(), "started.signal")
	// The trap is installed before the started marker is written, so a
	// reader that observes the started marker knows the TERM handler is
	// already armed. The foreground `while:; do sleep 1; done` keeps the
	// shell alive between sleeps so the deferred TERM trap runs within a
	// sleep tick of signal delivery — well inside the assertion window
	// below — which is the loop shape proven to deliver the marker under
	// /bin/sh here; the flake this test fought lived in the cancel timing,
	// not the trap latency.
	fakeDocker := `#!/bin/sh
trap 'echo term > "$WDM_TERM_FILE"; exit 0' TERM
echo started > "$WDM_STARTED_FILE"
while :; do
  sleep 1
done
`

	useFakeDocker(t, fakeDocker)
	t.Setenv("WDM_TERM_FILE", termFile)
	t.Setenv("WDM_STARTED_FILE", startedFile)

	client, err := New()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	// cancelAtCh hands the moment of cancellation from the cancel
	// goroutine to the test goroutine so the assertion below measures the
	// cancel->return interval (the real SIGTERM discriminator) instead of
	// a whole-test wall-clock bound that would also count child startup.
	// It is 1-buffered so the send never blocks the goroutine.
	cancelAtCh := make(chan time.Time, 1)
	// Cancel only once the child has demonstrably reached its armed state
	// (started marker present), never on a short wall-clock fallback:
	// under a loaded race/shuffle suite the freshly forked shell can take
	// seconds just to execute its first line, and canceling before the
	// trap is installed lets SIGTERM kill the shell at its default
	// disposition so the marker is never written. Waiting for the started
	// marker makes SIGTERM land on an armed handler deterministically. The
	// poll runs in a separate goroutine, so it must not call into
	// require/t.FailNow (that is only valid on the test goroutine); it
	// hands the cancel timestamp to the test goroutine through cancelAtCh
	// strictly before calling cancel, so the timestamp is buffered by
	// the time Run can observe cancellation. cancel runs unconditionally
	// on every exit path, including the generous-deadline path, so a
	// genuine regression where the child never starts surfaces not here
	// but as a loud main-goroutine failure at the marker read below: Run
	// still returns fast, the interval stays small, and the missing marker
	// trips the Eventually assertion.
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, statErr := os.Stat(startedFile); statErr == nil {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancelAtCh <- time.Now()
		cancel()
	}()

	_, err = client.Run(ctx, VersionInvocation{})
	returnedAt := time.Now()

	require.Error(t, err)
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUserCanceled, typedErr.Code)
	// Receive the cancel timestamp only after the ErrCodeUserCanceled
	// assertion: that assertion proves cancellation fired, which proves
	// the strictly-prior buffered send already happened, so this receive
	// can never block. Had Run failed early for an unrelated reason, the
	// wrong error code would have FailNow'd the test above, before we
	// reach this receive — so no hang is possible on any path.
	cancelAt := <-cancelAtCh
	require.Less(t, returnedAt.Sub(cancelAt), 5*time.Second, "cancel->return interval exceeded the SIGTERM window; production likely regressed to the WaitDelay SIGKILL path")

	// Eventually as robustness only: Run returns only after execCmd.Run
	// reaps the shell, and the shell exits only after the trap's echo
	// completes, so POSIX read-after-write guarantees the marker is
	// already visible on the first poll. The poll is therefore zero-cost
	// defense-in-depth on the pass path; it never tolerates a missing or
	// wrong-content marker, which is exactly the SIGKILL regression this
	// test must keep catching.
	require.Eventually(t, func() bool {
		signalBytes, readErr := os.ReadFile(termFile)
		return readErr == nil && string(signalBytes) == "term\n"
	}, 5*time.Second, 10*time.Millisecond, "TERM handler did not write the expected marker")
}

func TestValidateCommandSpec_RejectsUnsafeShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cmd  commandSpec
	}{
		{
			name: "empty argv",
			cmd:  commandSpec{argv: nil},
		},
		{
			name: "blank token",
			cmd:  commandSpec{argv: []string{"compose", "", "version"}},
		},
		{
			name: "unknown command",
			cmd:  commandSpec{argv: []string{"compose", "up", "-d"}},
		},
		{
			name: "compose down with -v",
			cmd:  commandSpec{argv: []string{"compose", "down", "-v"}},
		},
		{
			name: "shell form",
			cmd:  commandSpec{argv: []string{"sh", "-c", "docker version"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateCommandSpec(tt.cmd)
			require.Error(t, err)

			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
		})
	}
}

func TestValidateCommandSpec_AllowsCommit19Surface(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateCommandSpec(commandSpec{argv: []string{"version"}}))
	require.NoError(t, validateCommandSpec(commandSpec{argv: []string{"compose", "version"}}))
}

func TestRun_DefaultExecutorMapsMissingDockerBinaryToDockerUnavailable(t *testing.T) {
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	client, err := New()
	require.NoError(t, err)

	_, err = client.Run(t.Context(), VersionInvocation{})
	requireTypedCode(t, err, types.ErrCodeDockerUnavailable)
}

func TestRun_DefaultExecutorMapsDaemonUnreachableToDockerUnavailable(t *testing.T) {
	fakeDocker := `#!/bin/sh
echo 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?' >&2
exit 1
`
	useFakeDocker(t, fakeDocker)

	client, err := New()
	require.NoError(t, err)

	_, err = client.Run(t.Context(), VersionInvocation{})
	requireTypedCode(t, err, types.ErrCodeDockerUnavailable)
}

func TestRun_DefaultExecutorMapsNetworkStderrToNetworkFailure(t *testing.T) {
	fakeDocker := `#!/bin/sh
echo 'dial tcp: lookup registry-1.docker.io: no such host' >&2
exit 1
`
	useFakeDocker(t, fakeDocker)

	client, err := New()
	require.NoError(t, err)

	_, err = client.Run(t.Context(), VersionInvocation{})
	requireTypedCode(t, err, types.ErrCodeNetworkFailure)
}

func TestRun_DefaultExecutorMapsLockStderrToRuntimeLockHeld(t *testing.T) {
	fakeDocker := `#!/bin/sh
echo 'another operation is already in progress' >&2
exit 1
`
	useFakeDocker(t, fakeDocker)

	project := validComposeProjectForDeployTests(t)
	client, err := New()
	require.NoError(t, err)

	_, err = client.Run(t.Context(), composeDownInvocation{
		composeFile: project.ComposeFile,
		envFile:     project.EnvFile,
		projectName: project.ProjectName,
	})
	requireTypedCode(t, err, types.ErrCodeRuntimeLockHeld)
}

func TestRun_DefaultExecutorMapsUnknownExitToGenericAndRedactsErrorPath(t *testing.T) {
	secret := "top-secret"
	fakeDocker := `#!/bin/sh
echo 'failure secret=` + secret + `' >&2
exit 42
`
	useFakeDocker(t, fakeDocker)

	client, err := New(WithRedactor(security.NewActiveRedactor([]string{secret})))
	require.NoError(t, err)

	res, err := client.Run(t.Context(), VersionInvocation{})
	require.Error(t, err)
	require.Equal(t, 42, res.ExitCode)
	require.Contains(t, res.Stderr, security.RedactedPlaceholder)
	require.NotContains(t, res.Stderr, secret)
	require.NotContains(t, err.Error(), secret)
	requireTypedCode(t, err, types.ErrCodeGeneric)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.NotNil(t, typedErr.Cause)
	require.NotContains(t, typedErr.Cause.Error(), secret)

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
}

func TestProductionSourcesRejectShellComposeV1AndDangerousLiterals(t *testing.T) {
	t.Parallel()

	forbiddenComposeV1 := "docker" + "-compose"
	forbiddenShell := "sh" + " " + "-c"
	forbiddenDownV := "docker" + " " + "compose" + " " + "down" + " " + "-v"
	forbiddenNetworkRemove := "docker" + " " + "network" + " " + "rm"
	forbiddenNetworkPrune := "docker" + " " + "network" + " " + "prune"
	forbiddenVolumeRemove := "docker" + " " + "volume" + " " + "rm"
	forbiddenVolumePrune := "docker" + " " + "volume" + " " + "prune"
	forbiddenImageRemove := "docker" + " " + "image" + " " + "rm"
	forbiddenImagePrune := "docker" + " " + "image" + " " + "prune"
	forbiddenContainerRemove := "docker" + " " + "container" + " " + "rm"
	forbiddenContainerPrune := "docker" + " " + "container" + " " + "prune"

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fileSet := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "doc.go" {
			continue
		}

		raw, readErr := os.ReadFile(name)
		require.NoError(t, readErr)
		require.NotContains(t, string(raw), forbiddenComposeV1, "%s includes forbidden compose v1 literal", name)
		require.NotContains(t, string(raw), forbiddenDownV, "%s includes forbidden destructive literal", name)
		require.NotContains(t, string(raw), forbiddenNetworkRemove, "%s includes forbidden network remove literal", name)
		require.NotContains(t, string(raw), forbiddenNetworkPrune, "%s includes forbidden network prune literal", name)
		require.NotContains(t, string(raw), forbiddenVolumeRemove, "%s includes forbidden volume remove literal", name)
		require.NotContains(t, string(raw), forbiddenVolumePrune, "%s includes forbidden volume prune literal", name)
		require.NotContains(t, string(raw), forbiddenImageRemove, "%s includes forbidden image remove literal", name)
		require.NotContains(t, string(raw), forbiddenImagePrune, "%s includes forbidden image prune literal", name)
		require.NotContains(t, string(raw), forbiddenContainerRemove, "%s includes forbidden container remove literal", name)
		require.NotContains(t, string(raw), forbiddenContainerPrune, "%s includes forbidden container prune literal", name)

		file, parseErr := parser.ParseFile(fileSet, name, raw, parser.SkipObjectResolution)
		require.NoError(t, parseErr, "parsing %s", name)

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			if callHasAdjacentStringArgs(call, "sh", "-c") {
				t.Errorf("%s contains shell invocation via %q", name, forbiddenShell)
			}
			return true
		})
	}
}

func useFakeDocker(t *testing.T, scriptBody string) {
	t.Helper()

	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	require.NoError(t, os.WriteFile(dockerPath, []byte(scriptBody), 0o755))

	currentPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+currentPath)
}

func requireTypedCode(t *testing.T, err error, want types.ErrorCode) {
	t.Helper()
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, want, typedErr.Code)
}

func stringLiteralValue(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

func callHasAdjacentStringArgs(call *ast.CallExpr, first, second string) bool {
	for i := range len(call.Args) - 1 {
		gotFirst, firstOK := stringLiteralValue(call.Args[i])
		gotSecond, secondOK := stringLiteralValue(call.Args[i+1])
		if firstOK && secondOK && gotFirst == first && gotSecond == second {
			return true
		}
	}
	return false
}
