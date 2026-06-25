package core_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/wnstify/wdm/internal/core"
)

// TestResolveDockerSocketSource proves the rootless socket-source resolution
// (issue #134): $XDG_RUNTIME_DIR wins when set, otherwise the per-uid
// /run/user/<uid>/docker.sock fallback is used. t.Setenv forbids t.Parallel.
func TestResolveDockerSocketSource(t *testing.T) {
	t.Run("xdg runtime dir wins", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/4242")
		assert.Equal(t, "/run/user/4242/docker.sock", core.ResolveDockerSocketSourceForTest())
	})

	t.Run("falls back to per-uid socket", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		want := fmt.Sprintf("/run/user/%d/docker.sock", os.Getuid())
		assert.Equal(t, want, core.ResolveDockerSocketSourceForTest())
	})
}
