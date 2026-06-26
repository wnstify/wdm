package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/pkg/types"
)

// TestRequireRootlessDaemon_RefusesRootfulDaemon exercises the real PRD §11
// refusal through the genuine docker.IsRootlessDaemon parser, faking only the
// `docker info` transport. A rootless daemon passes, a rootful one is refused
// with a typed usage error, and a probe error is propagated unchanged.
func TestRequireRootlessDaemon_RefusesRootfulDaemon(t *testing.T) {
	probeErr := errors.New("daemon unreachable")

	tests := []struct {
		name    string
		stdout  string
		runErr  error
		wantErr bool
		wantIs  error
	}{
		{
			name:   "rootless daemon passes",
			stdout: `["name=seccomp","name=rootless"]`,
		},
		{
			name:    "rootful daemon refused",
			stdout:  `["name=seccomp"]`,
			wantErr: true,
		},
		{
			name:    "probe error propagated",
			runErr:  probeErr,
			wantErr: true,
			wantIs:  probeErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeDockerClient{
				runFn: func(_ int, _ docker.Invocation) (docker.CommandResult, error) {
					if tt.runErr != nil {
						return docker.CommandResult{}, tt.runErr
					}
					return docker.CommandResult{Stdout: tt.stdout}, nil
				},
			}
			restore := core.SetRootlessDaemonClientFactoryForTest(
				func() (docker.Client, error) { return fake, nil },
			)
			defer restore()

			err := core.RequireRootlessDaemon(context.Background())
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if tt.wantIs != nil {
				if !errors.Is(err, tt.wantIs) {
					t.Fatalf("want errors.Is %v, got %v", tt.wantIs, err)
				}
				return
			}
			var typed *types.Error
			if !errors.As(err, &typed) || typed.Code != types.ErrCodeUsageValidation {
				t.Fatalf("want typed ErrCodeUsageValidation, got %v", err)
			}
		})
	}
}
