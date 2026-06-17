package core_test

import (
	"context"
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/pkg/types"
)

// TestPortConflict_RenderTimeRefusalWritesNoFiles proves the first of
// the two row-34 port checkpoints end-to-end through the real
// Engine.Install API: when a required localhost port is
// already bound at the moment planning runs, Install refuses with a
// typed ErrCodeUsageValidation naming the port in the hint AND leaves
// zero files behind — the stack directory is never created because the
// refusal precedes the file-write stage. The confirmer is wired but
// must never be consulted (the render-time check fails before the
// confirm point), and the docker client is never constructed.
// The occupied port is derived from a real listener bound to an
// ephemeral 127.0.0.1 address, so the test stays parallel-safe and
// deterministic — it never races for a fixed port.
func TestPortConflict_RenderTimeRefusalWritesNoFiles(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	port := ln.Addr().(*net.TCPAddr).Port
	app := appFixture("port-conflict-render-app", port)
	eng, stackPath := newInstallDeployTestEngine(t, app)
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	confirmer := &fakeConfirmer{}
	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	assert.NotErrorIs(t, err, types.ErrNotImplemented)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
	assert.Contains(t, typedErr.Hint, strconv.Itoa(port),
		"the render-time conflict must name the occupied port in the hint")
	assert.NoDirExists(t, stackPath,
		"a render-time port conflict must not create the stack directory")
	assert.Empty(t, confirmer.calls,
		"a render-time refusal must never reach the deployment confirmation")
	assert.Zero(t, fake.calls,
		"a render-time refusal must run zero docker commands")
}

// TestPortConflict_PreDeployCheckpointNamesPortAndCleansUp proves the
// SECOND row-34 checkpoint — the TOCTOU close immediately before
// docker compose up -d. The port is free when planning
// runs (so the first checkpoint passes), then becomes occupied inside
// the deployment confirmation callback, exactly modeling the
// time-of-check/time-of-use window. Install must detect the now-bound
// port at the pre-deploy re-check, refuse with ErrCodeUsageValidation
// naming the port in the HINT specifically (not merely the message),
// and the fresh-install sad path must remove the partial files so the
// stack directory does not exist after the failure. This deliberately
// mirrors install_test.go's
// TestInstall_PortRecheckConflictBeforeDeploymentFailsClosed so the
// row-34 arc reads as a self-contained TestPortConflict_ pair alongside
// checkpoint 1.
// The fixture app declares no networks, so the compose-config
// validation is the single client call before the re-check refusal;
// the confirmation and the (empty) network pre-creation contribute
// none.
func TestPortConflict_PreDeployCheckpointNamesPortAndCleansUp(t *testing.T) {
	t.Parallel()

	port := freeLocalTCPPort(t)
	app := appFixture("port-conflict-toctou-app", port)
	eng, stackPath := newInstallDeployTestEngine(t, app)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	// Occupy the port inside the confirm callback: planning already
	// passed the first check, so this binding is observed only by the
	// pre-deployment re-check.
	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			require.NoError(t, err)
			t.Cleanup(func() { _ = ln.Close() })
			return true, nil
		},
	}

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	assert.NotErrorIs(t, err, types.ErrNotImplemented)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
	assert.Contains(t, typedErr.Hint, strconv.Itoa(port),
		"the pre-deploy TOCTOU conflict must name the occupied port in the hint")
	assert.Equal(t, 1, fake.calls,
		"only the pre-deploy compose-config validation runs before the re-check refusal")
	assert.NoDirExists(t, stackPath,
		"the fresh-install sad path must remove partial files after a pre-deploy port conflict")
}

// bindError reproduces the wrapped-error shape a failed
// net.ListenConfig.Listen returns on Linux — net.OpError wrapping
// os.SyscallError wrapping a syscall.Errno — so the classifier under
// test sees the same chain errors.Is must walk through in production. A
// real sub-1024 bind is not portable (macOS allows unprivileged low
// ports, CI may run as root), so the EACCES path is exercised here with
// a constructed error rather than an actual privileged bind.
func bindError(errno syscall.Errno) error {
	return &net.OpError{
		Op:   "listen",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 88},
		Err:  os.NewSyscallError("bind", errno),
	}
}

// TestClassifyPortBindError_DistinguishesEACCESFromInUse pins the
// EACCES-vs-already-in-use split. An EACCES bind (a sub-1024 host port
// that wdm — running unprivileged per PRD §11 — cannot bind) reports
// honestly that the port needs elevated privileges and points at an
// unprivileged (>1024) port, while EADDRINUSE and any other bind
// failure keep the byte-compatible already-in-use message. Both arms
// carry ErrCodeUsageValidation so the PRD §27 exit-code mapping is
// unchanged, both name the port in the hint, and both keep the
// underlying cause reachable via errors.Is.
func TestClassifyPortBindError_DistinguishesEACCESFromInUse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cause       error
		wantMessage string
		wantHint    string
		wantErrno   syscall.Errno
	}{
		{
			name:        "eacces reports elevated privileges",
			cause:       bindError(syscall.EACCES),
			wantMessage: "local port requires elevated privileges",
			wantHint:    "127.0.0.1:88 needs elevated privileges to bind; choose an unprivileged port above 1024",
			wantErrno:   syscall.EACCES,
		},
		{
			name:        "eaddrinuse keeps already-in-use message",
			cause:       bindError(syscall.EADDRINUSE),
			wantMessage: "local port is already in use",
			wantHint:    "free 127.0.0.1:88 or choose another port",
			wantErrno:   syscall.EADDRINUSE,
		},
		{
			name:        "unknown bind failure keeps already-in-use message",
			cause:       bindError(syscall.EADDRNOTAVAIL),
			wantMessage: "local port is already in use",
			wantHint:    "free 127.0.0.1:88 or choose another port",
			wantErrno:   syscall.EADDRNOTAVAIL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := core.ClassifyPortBindErrorForTest(88, tt.cause)
			require.Error(t, err)

			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
			assert.Equal(t, tt.wantMessage, typedErr.Message)
			assert.Equal(t, tt.wantHint, typedErr.Hint)
			assert.ErrorIs(t, err, tt.wantErrno,
				"the underlying syscall cause must stay reachable via errors.Is")
		})
	}
}
