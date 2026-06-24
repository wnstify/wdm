package engine

import (
	"strings"

	"github.com/wnstify/wdm/pkg/types"
)

// ResolveEditorArgv builds the typed argv for launching the user's editor on
// path, following the standard precedence $VISUAL → $EDITOR → nano. The chosen
// value is field-split on whitespace so an "EDITOR=code -w" style value yields
// the binary plus its flags as separate argv elements, and path is appended as
// the final argument. The result is a typed argv slice — never a shell string —
// so a value containing shell metacharacters stays a single literal argument
// and is never interpreted by a shell.
//
// It is a pure package function (no Engine state) so BOTH the CLI and TUI may
// resolve the editor without importing internal/system: callers pass the
// os.Getenv("VISUAL") and os.Getenv("EDITOR") values. nano is the always-set
// fallback, so an error is returned only if the resolved value field-splits to
// nothing — a defensive guard that should not occur given the default.
func ResolveEditorArgv(visual, editor, path string) ([]string, error) {
	chosen := strings.TrimSpace(visual)
	if chosen == "" {
		chosen = strings.TrimSpace(editor)
	}
	if chosen == "" {
		chosen = "nano"
	}

	fields := strings.Fields(chosen)
	if len(fields) == 0 {
		return nil, types.NewError(
			types.ErrCodeUsageValidation,
			"no editor could be resolved",
			"set $VISUAL or $EDITOR to an editor command, or ensure nano is available",
		)
	}

	argv := make([]string, 0, len(fields)+1)
	argv = append(argv, fields...)
	argv = append(argv, path)
	return argv, nil
}
