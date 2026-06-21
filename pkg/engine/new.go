package engine

import (
	"fmt"

	"github.com/wnstify/wdm/internal/core"
)

// New constructs an [Engine] from the supplied [Option] values. PRD §29
// names this the GUI-facing constructor;
// locks the signature. It validates the resolved config eagerly and
// returns a wrapped [types.ErrConfigInvalid] on schema failure.
// into the matching [core.Option] and delegates construction to
// [core.New], so cmd/wdm and any future GUI use pkg/engine.New as the
// single entry point rather than reaching into internal/core. The
// depguard "pkg-engine-stays-uiless" rule scopes the boundary:
// pkg/engine may import internal/core only, never any other internal/*
// sibling.
// Errors from either layer (option validation here, or core construction
// below) carry the "engine.New:" prefix so callers can tell construction
// failures from later operation failures. The double-wrap "engine.New:
// core.New:..." preserves errors.Is/errors.As traversal, keeping
// sentinels like [types.ErrConfigInvalid] detectable through the chain.
func New(opts ...Option) (Engine, error) {
	cfg := &config{}
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("engine.New: %w", err)
		}
	}

	var coreOpts []core.Option
	if cfg.stateDir != "" {
		coreOpts = append(coreOpts, core.WithStateDir(cfg.stateDir))
	}
	if cfg.dataDir != "" {
		coreOpts = append(coreOpts, core.WithDataDir(cfg.dataDir))
	}
	if cfg.stackBaseDir != "" {
		coreOpts = append(coreOpts, core.WithStackBaseDir(cfg.stackBaseDir))
	}
	if cfg.configPath != "" {
		coreOpts = append(coreOpts, core.WithConfigPath(cfg.configPath))
	}
	if cfg.logger != nil {
		coreOpts = append(coreOpts, core.WithLogger(cfg.logger))
	}
	if cfg.fallbackLog != nil {
		coreOpts = append(coreOpts, core.WithFallbackLogWriter(cfg.fallbackLog))
	}
	if cfg.catalog != nil {
		coreOpts = append(coreOpts, core.WithCatalog(cfg.catalog))
	}
	if cfg.version != "" {
		coreOpts = append(coreOpts, core.WithVersion(cfg.version))
	}

	eng, err := core.New(coreOpts...)
	if err != nil {
		return nil, fmt.Errorf("engine.New: %w", err)
	}
	return eng, nil
}

// Compile-time check that *core.Engine satisfies [Engine]. The assertion
// lives at the bridge site because internal/core no longer imports
// pkg/engine; the build fails fast here
// if a method signature drifts on either side.
var _ Engine = (*core.Engine)(nil)
