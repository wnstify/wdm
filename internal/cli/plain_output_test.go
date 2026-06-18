package cli

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

type errorWriter struct {
	err    error
	writes int
}

func (w *errorWriter) Write(_ []byte) (int, error) {
	w.writes++
	return 0, w.err
}

func TestAppsStatus_Plain_RendersAttentionServicesPortsAndSnapshot(t *testing.T) {
	t.Parallel()

	checkedAt := time.Date(2026, time.June, 18, 3, 24, 0, 0, time.UTC)
	fake := &fakeEngine{
		statusResult: &types.AppStatus{
			AppID:            "vaultwarden",
			State:            "needs_attention",
			Message:          "post-check found issues",
			NeedsAttention:   true,
			AttentionReasons: []string{"container_exited", "port_unreachable"},
			Services: []types.ServiceStatus{
				{
					Service: "server",
					State:   "running",
					Health:  "healthy",
					PublishedPorts: []types.PortBinding{
						{
							HostIP:        "127.0.0.1",
							HostPort:      8080,
							ContainerPort: 80,
							Protocol:      "tcp",
						},
					},
				},
				{
					Service:        "worker",
					State:          "exited",
					NeedsAttention: true,
					Message:        "container exited with status 1",
				},
			},
			UpdatedAt: &checkedAt,
		},
	}

	stdout, stderr, err := runLeaf(t, fake, "apps", "status", "vaultwarden")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "vaultwarden", fake.statusAppID)
	assert.Contains(t, stdout, "vaultwarden\tneeds_attention\n")
	assert.Contains(t, stdout, "  post-check found issues\n")
	assert.Contains(t, stdout, "\nNeeds attention:\n")
	assert.Contains(t, stdout, "  - container_exited\n")
	assert.Contains(t, stdout, "  - port_unreachable\n")
	assert.Contains(t, stdout, "\nServices:\n")
	assert.Contains(t, stdout, "  server\trunning (healthy)\n")
	assert.Contains(t, stdout, "      127.0.0.1:8080 -> 80/tcp\n")
	assert.Contains(t, stdout, "  worker\texited !\n")
	assert.Contains(t, stdout, "      container exited with status 1\n")
	assert.Contains(t, stdout, "\nChecked at 2026-06-18 03:24:00 UTC\n")
	assert.NotContains(t, stdout, `"schema"`)
}

func TestAppsInstall_Plain_RendersGuidanceAndOneTimeCredentials(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		installResult: &types.InstallResult{
			AppID:     "vaultwarden",
			StackPath: "/home/operator/docker/vaultwarden",
			LocalPorts: []types.PortBinding{
				{
					Service:  "server",
					HostIP:   "127.0.0.1",
					HostPort: 8080,
				},
			},
			PostInstallGuidance: &types.PostInstallGuidance{
				LocalTargetURL: "http://127.0.0.1:8080",
				Pangolin: &types.PangolinGuidance{
					RecommendedSubdomain: "vaultwarden",
					TargetURL:            "http://127.0.0.1:8080",
					Notes:                []string{"Set the service type to HTTP."},
				},
				FirstRunNotes: []string{"Create the first admin user before exposing the app."},
				GeneratedCredentials: []types.GeneratedCredential{
					{
						Label: "Vaultwarden ADMIN_TOKEN",
						Value: "admin-token-plaintext",
						Note:  "Store this value now.",
					},
				},
			},
		},
	}

	stdout, stderr, err := runLeaf(t, fake, "apps", "install", "vaultwarden", "--yes")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.False(t, fake.progressWasNil)
	assert.Equal(t, types.InstallRequest{AppID: "vaultwarden"}, fake.installReq)
	assert.Contains(t, stdout, "vaultwarden is installed at /home/operator/docker/vaultwarden\n")
	assert.Contains(t, stdout, "\nRunning locally at:\n  http://127.0.0.1:8080\n")
	assert.Contains(t, stdout, "\nBound ports:\n  127.0.0.1:8080 -> server\n")
	assert.Contains(t, stdout, "\nRecommended: expose with Pangolin\n")
	assert.Contains(t, stdout, "  subdomain: vaultwarden\n")
	assert.Contains(t, stdout, "  point it to: http://127.0.0.1:8080\n")
	assert.Contains(t, stdout, "  Set the service type to HTTP.\n")
	assert.Contains(t, stdout, "\nFirst run:\n")
	assert.Contains(t, stdout, "  Create the first admin user before exposing the app.\n")
	assert.Contains(t, stdout, "SAVE THIS NOW")
	assert.Contains(t, stdout, "shown once, cannot be recovered")
	assert.Contains(t, stdout, "  Vaultwarden ADMIN_TOKEN:\n")
	assert.Contains(t, stdout, "    admin-token-plaintext\n")
	assert.Contains(t, stdout, "    Store this value now.\n")
	assert.NotContains(t, stdout, `"schema"`)
}

func TestAppsLogs_Plain_RendersStableLinesAndMapsFlags(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.June, 18, 3, 24, 0, 0, time.UTC)
	second := first.Add(time.Second)
	fake := &fakeEngine{
		logsLines: []types.LogLine{
			{
				Timestamp: first,
				Service:   "server",
				Stream:    "stdout",
				Text:      "started",
			},
			{
				Timestamp: second,
				Service:   "db",
				Text:      "ready",
			},
		},
	}

	stdout, stderr, err := runLeaf(
		t,
		fake,
		"apps",
		"logs",
		"vaultwarden",
		"--tail",
		"20",
		"--service",
		"server",
		"--service",
		"db",
	)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "vaultwarden", fake.logsReq.AppID)
	assert.False(t, fake.logsReq.Follow)
	assert.Equal(t, 20, fake.logsReq.Tail)
	assert.Equal(t, []string{"server", "db"}, fake.logsReq.Services)
	assert.False(t, fake.logLineWasNil)
	assert.Equal(t, "2026-06-18T03:24:00Z server stdout | started\n2026-06-18T03:24:01Z db | ready\n", stdout)
}

func TestAppsRemove_Plain_RendersPreservedResourcesAndStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		result            *types.RemoveResult
		expectedFragments []string
	}{
		{
			name: "clean removal with preserved Docker resources",
			result: &types.RemoveResult{
				AppID:          "vaultwarden",
				StackPath:      "/home/operator/docker/vaultwarden",
				PreservedPaths: []string{"/home/operator/docker/vaultwarden/data"},
				RemainingNamedVolumes: []string{
					"wdm-vaultwarden_data",
				},
				RemainingNetworks: []string{"pangolin-public"},
				Status: &types.AppStatus{
					State: "removed",
				},
			},
			expectedFragments: []string{
				"vaultwarden is removed from /home/operator/docker/vaultwarden\n",
				"Containers were stopped and removed. Files and data were kept.\n",
				"\nKept on disk:\n  - /home/operator/docker/vaultwarden/data\n",
				"\nNamed volumes:\n  - wdm-vaultwarden_data\n",
				"\nNetworks left in place:\n  - pangolin-public\n",
				"\nStatus: removed\n",
			},
		},
		{
			name: "attention status keeps neutral headline and empty-volume wording",
			result: &types.RemoveResult{
				AppID:     "vaultwarden",
				StackPath: "/home/operator/docker/vaultwarden",
				Status: &types.AppStatus{
					State:          "needs_attention",
					Message:        "one container still appears in Docker",
					NeedsAttention: true,
				},
			},
			expectedFragments: []string{
				"Removal is recorded. Files and data were kept; see the status below for containers that may remain.\n",
				"\nNamed volumes:\n  none reported (Docker inspection data may be unavailable)\n",
				"\nStatus: needs_attention\n",
				"  one container still appears in Docker\n",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{removeResult: tt.result}
			stdout, stderr, err := runLeaf(t, fake, "apps", "remove", "vaultwarden", "--yes")
			require.NoError(t, err)
			assert.Empty(t, stderr)
			assert.False(t, fake.progressWasNil)
			assert.Equal(t, types.RemoveRequest{AppID: "vaultwarden"}, fake.removeReq)
			for _, fragment := range tt.expectedFragments {
				assert.Contains(t, stdout, fragment)
			}
			assert.NotContains(t, stdout, `"schema"`)
		})
	}
}

func TestAppsUpdate_Plain_RendersDryRunApplyAndUpToDateReports(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		args              []string
		result            *types.UpdateResult
		wantReq           types.UpdateRequest
		expectedFragments []string
		rejectedFragments []string
	}{
		{
			name: "dry run reports available update without apply-only fields",
			args: []string{"apps", "update", "vaultwarden", "--dry-run"},
			result: &types.UpdateResult{
				AppID:                   "vaultwarden",
				PreviousTemplateVersion: "1.0.0",
				NewTemplateVersion:      "1.1.0",
				UpdatedServices:         []string{"server ghcr.io/example/server:1.0.0 -> ghcr.io/example/server:1.1.0"},
				RiskClassifications:     []string{"config", "image"},
			},
			wantReq: types.UpdateRequest{AppID: "vaultwarden", DryRun: true},
			expectedFragments: []string{
				"vaultwarden\tupdate available\t1.0.0 -> 1.1.0\n",
				"\nImage changes:\n  - server ghcr.io/example/server:1.0.0 -> ghcr.io/example/server:1.1.0\n",
				"\nRisk: config, image\n",
			},
			rejectedFragments: []string{"Config backup:", "Status:"},
		},
		{
			name: "apply report includes backup and post-update status",
			args: []string{"apps", "update", "vaultwarden", "--yes"},
			result: &types.UpdateResult{
				AppID:                   "vaultwarden",
				PreviousTemplateVersion: "1.0.0",
				NewTemplateVersion:      "1.1.0",
				UpdatedServices:         []string{"server"},
				BackupPath:              "/home/operator/docker/vaultwarden/.wdm-backups/20260618-update",
				Status: &types.AppStatus{
					State: "running",
				},
			},
			wantReq: types.UpdateRequest{AppID: "vaultwarden"},
			expectedFragments: []string{
				"vaultwarden\tupdated\t1.0.0 -> 1.1.0\n",
				"\nImage changes:\n  - server\n",
				"\nConfig backup: /home/operator/docker/vaultwarden/.wdm-backups/20260618-update\n",
				"Status: running\n",
			},
		},
		{
			name: "no-op report says up to date",
			args: []string{"apps", "update", "vaultwarden", "--dry-run"},
			result: &types.UpdateResult{
				AppID:                   "vaultwarden",
				PreviousTemplateVersion: "1.1.0",
				NewTemplateVersion:      "1.1.0",
			},
			wantReq: types.UpdateRequest{AppID: "vaultwarden", DryRun: true},
			expectedFragments: []string{
				"vaultwarden\tup to date\t1.1.0\n",
			},
			rejectedFragments: []string{"Image changes:", "Risk:", "Config backup:", "Status:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{updateResult: tt.result}
			stdout, stderr, err := runLeaf(t, fake, tt.args...)
			require.NoError(t, err)
			assert.Empty(t, stderr)
			assert.Equal(t, tt.wantReq, fake.updateReq)
			assert.False(t, fake.progressWasNil)
			for _, fragment := range tt.expectedFragments {
				assert.Contains(t, stdout, fragment)
			}
			for _, fragment := range tt.rejectedFragments {
				assert.NotContains(t, stdout, fragment)
			}
			assert.NotContains(t, stdout, `"schema"`)
		})
	}
}

func TestLogSink_RecordsFirstWriteErrorAndDropsLaterLines(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("disk full")
	for _, tt := range []struct {
		name          string
		useJSON       bool
		errorContains string
	}{
		{
			name:          "plain line",
			errorContains: "apps logs: writing log line",
		},
		{
			name:          "json envelope",
			useJSON:       true,
			errorContains: "cli: encoding envelope",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			writer := &errorWriter{err: writeErr}
			sink := newLogSink(writer, tt.useJSON)
			line := types.LogLine{
				Timestamp: time.Date(2026, time.June, 18, 3, 24, 0, 0, time.UTC),
				Service:   "server",
				Stream:    "stdout",
				Text:      "started",
			}

			sink.emit(line)
			sink.emit(line)

			require.Error(t, sink.err)
			assert.ErrorIs(t, sink.err, writeErr)
			assert.ErrorContains(t, sink.err, tt.errorContains)
			assert.Equal(t, 1, writer.writes)
		})
	}
}
