package engine_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
)

// TestResolveEditorArgv covers the precedence chain ($VISUAL → $EDITOR →
// nano), whitespace field-splitting of the chosen value, and the no-shell
// guarantee: a value containing shell metacharacters stays a single literal
// argv token and is never interpreted.
func TestResolveEditorArgv(t *testing.T) {
	t.Parallel()

	const path = "/home/u/docker/app/.env.user"

	tests := []struct {
		name     string
		visual   string
		editor   string
		expected []string
	}{
		{
			name:     "visual wins over editor and default",
			visual:   "vim",
			editor:   "emacs",
			expected: []string{"vim", path},
		},
		{
			name:     "editor used when visual empty",
			visual:   "",
			editor:   "emacs",
			expected: []string{"emacs", path},
		},
		{
			name:     "nano default when both empty",
			visual:   "",
			editor:   "",
			expected: []string{"nano", path},
		},
		{
			name:     "whitespace-only values fall through to nano",
			visual:   "   ",
			editor:   "\t",
			expected: []string{"nano", path},
		},
		{
			name:     "field-split splits binary and flags",
			visual:   "code -w",
			editor:   "",
			expected: []string{"code", "-w", path},
		},
		{
			name:     "field-split collapses repeated whitespace",
			visual:   "  code   --wait  ",
			editor:   "",
			expected: []string{"code", "--wait", path},
		},
		{
			name:   "shell metacharacters stay literal argv tokens",
			visual: "evil;rm -rf /",
			editor: "",
			// Field-split treats ";rm" and "-rf" and "/" as separate plain
			// args; nothing is shell-interpreted. The metacharacter ";" never
			// becomes a command separator.
			expected: []string{"evil;rm", "-rf", "/", path},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			argv, err := engine.ResolveEditorArgv(tt.visual, tt.editor, path)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, argv)
		})
	}
}
