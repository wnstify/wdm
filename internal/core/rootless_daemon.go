package core

import (
	"context"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

// rootlessDaemonClientFactory builds the read-only Docker client the
// daemon-mode probe uses (PRD §11). It is a package var so tests can inject a
// fake transport instead of shelling out to `docker info`; production reuses
// the same structural-redaction client factory as every other Docker call.
var rootlessDaemonClientFactory = func() (docker.Client, error) {
	return defaultDockerClientFactory(security.NewActiveRedactor(nil))
}

// RequireRootlessDaemon refuses fail-closed unless the active Docker daemon
// runs rootless (PRD §11, issue #135). cmd/wdm calls it through
// [pkg/engine.RequireRootlessDaemon] from the engine factories, so every real
// command and the TUI entry refuse a rootful or indeterminate daemon before
// the engine is built — while `wdm --version`/`--help`, which never construct
// an engine, stay Docker-free. A probe error (daemon unreachable, unparseable
// info) is propagated unchanged so it keeps its typed exit code and still
// fails closed. There is no override flag.
func RequireRootlessDaemon(ctx context.Context) error {
	client, err := rootlessDaemonClientFactory()
	if err != nil {
		return err
	}

	rootless, err := docker.IsRootlessDaemon(ctx, client)
	if err != nil {
		return err
	}
	if !rootless {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"wdm requires rootless Docker; the active daemon is running in rootful mode",
			"set up rootless Docker (see scripts/ops/provision-rootless-docker-user.sh), point your Docker context at it, and retry",
		)
	}
	return nil
}
