package engine

import "github.com/wnstify/wdm/pkg/types"

// ProgressFn is an alias for [types.ProgressFn] (PRD §37). The concrete
// type moved to pkg/types at the engine-bridge slice
// to break the pkg/engine ↔ internal/core import cycle, letting [New]
// delegate to core.New. The alias keeps engine.ProgressFn callers
// source-compatible.
type ProgressFn = types.ProgressFn

// LogLineFn is an alias for [types.LogLineFn]. See [ProgressFn] for
// the relocation rationale.
type LogLineFn = types.LogLineFn

// Confirmer is an alias for [types.Confirmer]. See [ProgressFn] for
// the relocation rationale.
type Confirmer = types.Confirmer
