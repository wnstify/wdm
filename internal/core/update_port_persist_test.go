package core_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// updatePortCandidateTmpl builds a candidate compose template that binds a
// single loopback host port (the new catalog default) while keeping the
// fixture's sidecar mount and env block so the render passes mount and leak
// verification. portsLine is the short-form host_ip:host:container spec.
func updatePortCandidateTmpl(portsLine string) string {
	return `services:
  app:
    image: docker.io/example/app:2.0.0
    ports:
      - "` + portsLine + `"
    volumes:
      - ./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
      API_TOKEN: ${API_TOKEN}
      SITE_NAME: ${SITE_NAME}
`
}

// updatePortDiskCompose builds the on-disk (pre-render) compose the update
// reads to recover the effective host-port binding.
func updatePortDiskCompose(portsLine string) string {
	return `services:
  app:
    image: docker.io/example/app:1.0.0
    ports:
      - "` + portsLine + `"
`
}

// newUpdatePortFixture wires an update apply fixture whose candidate template
// renders candidatePorts as the new catalog default and whose on-disk compose
// is overwritten with diskCompose (the effective pre-render bindings the
// preservation step reads).
func newUpdatePortFixture(t *testing.T, appID, candidateTmpl, diskCompose string) *updateApplyFixture {
	t.Helper()
	fx := newUpdateApplyFixture(t, updateApplyApp(appID), false, func(templates map[string]string) {
		templates["templates/"+appID+"/docker-compose.yml.tmpl"] = candidateTmpl
	}, nil)
	require.NoError(t, os.WriteFile(fx.composePath, []byte(diskCompose), 0o644))
	return fx
}

// TestUpdate_ApplyPreservesRemappedHostPort is the acceptance proof for issue
// #148: the on-disk compose binds the remapped loopback port 9999 while the
// new catalog default is 8080; after update the written compose must still
// bind 9999, not revert to 8080.
func TestUpdate_ApplyPreservesRemappedHostPort(t *testing.T) {
	t.Parallel()

	fx := newUpdatePortFixture(t, "preserve-port-app",
		updatePortCandidateTmpl("127.0.0.1:8080:80"),
		updatePortDiskCompose("127.0.0.1:9999:80"),
	)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Contains(t, string(composeAfter), "127.0.0.1:9999:80",
		"the remapped host port must survive the update re-render")
	assert.NotContains(t, string(composeAfter), "8080",
		"the catalog default must not revert the remapped port")
}

// TestUpdate_ApplyRecordedPortWinsOverChangedDefault proves the recorded host
// port wins even when the catalog maintainer changed the default across
// versions: on-disk binds 9999, the new candidate default is 8081, and 9999
// must survive.
func TestUpdate_ApplyRecordedPortWinsOverChangedDefault(t *testing.T) {
	t.Parallel()

	fx := newUpdatePortFixture(t, "changed-default-port-app",
		updatePortCandidateTmpl("127.0.0.1:8081:80"),
		updatePortDiskCompose("127.0.0.1:9999:80"),
	)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Contains(t, string(composeAfter), "127.0.0.1:9999:80",
		"the recorded host port wins over a changed catalog default")
	assert.NotContains(t, string(composeAfter), "8081",
		"the changed catalog default must not override the recorded port")
}

// TestUpdate_ApplyNonRemappedPortIsNoOp proves a binding whose effective host
// port equals the new catalog default renders the default unchanged (no
// spurious rewrite).
func TestUpdate_ApplyNonRemappedPortIsNoOp(t *testing.T) {
	t.Parallel()

	fx := newUpdatePortFixture(t, "noop-port-app",
		updatePortCandidateTmpl("127.0.0.1:8080:80"),
		updatePortDiskCompose("127.0.0.1:8080:80"),
	)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Contains(t, string(composeAfter), "127.0.0.1:8080:80",
		"an unchanged port renders the catalog default")
}

// TestUpdate_ApplyDroppedBindingIsSkipped proves a loopback binding whose
// (service, containerPort) the new candidate no longer declares is skipped
// without error and leaves no stray binding: the on-disk compose binds 9999
// on container port 80, but the new candidate declares no ports at all.
func TestUpdate_ApplyDroppedBindingIsSkipped(t *testing.T) {
	t.Parallel()

	// The default updateApplyApp candidate template declares no ports.
	app := updateApplyApp("dropped-binding-app")
	fx := newUpdateApplyFixture(t, app, false, nil, nil)
	require.NoError(t, os.WriteFile(fx.composePath,
		[]byte(updatePortDiskCompose("127.0.0.1:9999:80")), 0o644))

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.NotContains(t, string(composeAfter), "9999",
		"a binding the new catalog dropped must not be carried over")
}

// TestUpdate_ApplyUnparseableOnDiskComposeFailsClosed proves the preservation
// step fails closed: an unparseable on-disk compose surfaces a verification
// error and rolls back before any Docker call.
func TestUpdate_ApplyUnparseableOnDiskComposeFailsClosed(t *testing.T) {
	t.Parallel()

	fx := newUpdatePortFixture(t, "unparseable-port-app",
		updatePortCandidateTmpl("127.0.0.1:8080:80"),
		"services: [: not: valid: yaml\n  - broken",
	)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertVerificationFailed(t, err)
	assert.Zero(t, fx.fake.calls)
}

// TestReconfigure_PreservesRemappedHostPort is the guard slice (no production
// change): reconfigure reuses the on-disk compose verbatim, so a remapped
// loopback port already survives it. The written compose must still bind the
// remapped port after a resource-only reconfigure.
func TestReconfigure_PreservesRemappedHostPort(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-preserve-port-app"), nil)
	composePath := filepath.Join(fx.stackPath, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath,
		[]byte(updatePortDiskCompose("127.0.0.1:9999:80")), 0o644))

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	composeAfter, err := os.ReadFile(composePath)
	require.NoError(t, err)
	assert.Contains(t, string(composeAfter), "127.0.0.1:9999:80",
		"reconfigure must preserve a remapped host port (on-disk compose reused verbatim)")
}
