package types_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestInstallRequest_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.InstallRequest{
		AppID:             "vaultwarden",
		Domain:            "vault.example.com",
		StackPath:         "/home/test/docker/vaultwarden",
		PlaceholderValues: map[string]string{"SMTP_HOST": "smtp.example.com"},
		ResourceProfile:   types.ResourceProfileRecommended,
		ResourceOverrides: []types.ResourceOverride{
			{
				Service: "server",
				Memory:  "512m",
				CPUs:    "1.5",
				PIDs:    200,
			},
		},
	})

	assert.JSONEq(t, `{
		"app_id":"vaultwarden",
		"domain":"vault.example.com",
		"stack_path":"/home/test/docker/vaultwarden",
		"placeholder_values":{"SMTP_HOST":"smtp.example.com"},
		"resource_profile":"recommended",
		"resource_overrides":[{"service":"server","memory":"512m","cpus":"1.5","pids":200}]
	}`, got)
}

func TestInstallResult_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.May, 29, 12, 0, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.InstallResult{
		AppID:          "vaultwarden",
		StackPath:      "/home/test/docker/vaultwarden",
		ComposeProject: "wdm-vaultwarden",
		StartedServices: []string{
			"server",
			"db",
		},
		LocalPorts: []types.PortBinding{
			{
				Service:       "server",
				HostIP:        "127.0.0.1",
				HostPort:      8080,
				ContainerPort: 80,
				Protocol:      "tcp",
			},
		},
		PostInstallGuidance: &types.PostInstallGuidance{
			LocalTargetURL: "http://127.0.0.1:8080",
			Pangolin: &types.PangolinGuidance{
				TargetURL:            "http://127.0.0.1:8080",
				RecommendedSubdomain: "vault",
				Notes:                []string{"Use Pangolin for public access."},
			},
			FirstRunNotes: []string{"Finish first-run setup."},
		},
		Status: &types.AppStatus{
			AppID:            "vaultwarden",
			State:            "running",
			Message:          "ready",
			ComposeProject:   "wdm-vaultwarden",
			StackPath:        "/home/test/docker/vaultwarden",
			NeedsAttention:   false,
			AttentionReasons: []string{},
			Services: []types.ServiceStatus{
				{
					Service:       "server",
					ContainerName: "wdm-vaultwarden-server-1",
					State:         "running",
					Health:        "healthy",
				},
			},
			LocalPorts: []types.PortBinding{
				{
					Service:       "server",
					HostIP:        "127.0.0.1",
					HostPort:      8080,
					ContainerPort: 80,
					Protocol:      "tcp",
				},
			},
			UpdatedAt: &now,
		},
	})

	assert.JSONEq(t, `{
		"app_id":"vaultwarden",
		"stack_path":"/home/test/docker/vaultwarden",
		"compose_project":"wdm-vaultwarden",
		"started_services":["server","db"],
		"local_ports":[{"service":"server","host_ip":"127.0.0.1","host_port":8080,"container_port":80,"protocol":"tcp"}],
		"post_install_guidance":{
			"local_target_url":"http://127.0.0.1:8080",
			"pangolin_guidance":{
				"target_url":"http://127.0.0.1:8080",
				"recommended_subdomain":"vault",
				"notes":["Use Pangolin for public access."]
			},
			"first_run_notes":["Finish first-run setup."]
		},
		"status":{
			"app_id":"vaultwarden",
			"state":"running",
			"message":"ready",
			"compose_project":"wdm-vaultwarden",
			"stack_path":"/home/test/docker/vaultwarden",
			"needs_attention":false,
			"services":[{"service":"server","container_name":"wdm-vaultwarden-server-1","state":"running","health":"healthy"}],
			"local_ports":[{"service":"server","host_ip":"127.0.0.1","host_port":8080,"container_port":80,"protocol":"tcp"}],
			"updated_at":"2026-05-29T12:00:00Z"
		}
	}`, got)
}

func TestPostInstallGuidance_JSONContract_OmitsEmptyPangolinGuidance(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.PostInstallGuidance{
		LocalTargetURL: "http://127.0.0.1:8080",
	})

	assert.JSONEq(t, `{
		"local_target_url":"http://127.0.0.1:8080"
	}`, got)
}

func TestUpdateRequest_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.UpdateRequest{
		AppID:                 "vaultwarden",
		TargetTemplateVersion: "2.1.0",
		DryRun:                true,
	})

	assert.JSONEq(t, `{
		"app_id":"vaultwarden",
		"target_template_version":"2.1.0",
		"dry_run":true
	}`, got)
}

func TestUpdateResult_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	updatedAt := time.Date(2026, time.May, 29, 12, 5, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.UpdateResult{
		AppID:                   "vaultwarden",
		PreviousTemplateVersion: "2.0.0",
		NewTemplateVersion:      "2.1.0",
		UpdatedServices:         []string{"server"},
		RiskClassifications:     []string{"database"},
		BackupPath:              "/home/test/docker/vaultwarden/.wdm-backups/20260529T120000Z",
		Status: &types.AppStatus{
			AppID:          "vaultwarden",
			State:          "running",
			NeedsAttention: false,
			UpdatedAt:      &updatedAt,
		},
	})

	assert.JSONEq(t, `{
		"app_id":"vaultwarden",
		"previous_template_version":"2.0.0",
		"new_template_version":"2.1.0",
		"updated_services":["server"],
		"risk_classifications":["database"],
		"backup_path":"/home/test/docker/vaultwarden/.wdm-backups/20260529T120000Z",
		"status":{"app_id":"vaultwarden","state":"running","needs_attention":false,"updated_at":"2026-05-29T12:05:00Z"}
	}`, got)
}

func TestRemoveRequest_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.RemoveRequest{
		AppID:     "vaultwarden",
		StackPath: "/home/test/docker/vaultwarden",
	})

	assert.JSONEq(t, `{
		"app_id":"vaultwarden",
		"stack_path":"/home/test/docker/vaultwarden"
	}`, got)
}

func TestRemoveResult_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	updatedAt := time.Date(2026, time.May, 29, 12, 10, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.RemoveResult{
		AppID:                 "vaultwarden",
		StackPath:             "/home/test/docker/vaultwarden",
		ComposeProject:        "wdm-vaultwarden",
		PreservedPaths:        []string{"/home/test/docker/vaultwarden/.wdm-backups"},
		RemainingNamedVolumes: []string{"wdm-vaultwarden_db_data"},
		RemainingNetworks:     []string{"vaultwarden-net"},
		Status: &types.AppStatus{
			AppID:            "vaultwarden",
			State:            "needs_attention",
			NeedsAttention:   true,
			AttentionReasons: []string{"stack_removed"},
			UpdatedAt:        &updatedAt,
		},
	})

	assert.JSONEq(t, `{
		"app_id":"vaultwarden",
		"stack_path":"/home/test/docker/vaultwarden",
		"compose_project":"wdm-vaultwarden",
		"preserved_paths":["/home/test/docker/vaultwarden/.wdm-backups"],
		"remaining_named_volumes":["wdm-vaultwarden_db_data"],
		"remaining_networks":["vaultwarden-net"],
		"status":{
			"app_id":"vaultwarden",
			"state":"needs_attention",
			"needs_attention":true,
			"attention_reasons":["stack_removed"],
			"updated_at":"2026-05-29T12:10:00Z"
		}
	}`, got)
}

func TestAppStatus_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.May, 29, 14, 30, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.AppStatus{
		AppID:            "vaultwarden",
		State:            "needs_attention",
		Message:          "container unhealthy",
		ComposeProject:   "wdm-vaultwarden",
		StackPath:        "/home/test/docker/vaultwarden",
		NeedsAttention:   true,
		AttentionReasons: []string{"service_unhealthy"},
		Services: []types.ServiceStatus{
			{
				Service:        "server",
				State:          "running",
				Health:         "unhealthy",
				NeedsAttention: true,
				Message:        "healthcheck failed",
				PublishedPorts: []types.PortBinding{
					{
						Service:       "server",
						HostIP:        "127.0.0.1",
						HostPort:      8080,
						ContainerPort: 80,
						Protocol:      "tcp",
					},
				},
			},
		},
		LocalPorts: []types.PortBinding{
			{
				Service:       "server",
				HostIP:        "127.0.0.1",
				HostPort:      8080,
				ContainerPort: 80,
				Protocol:      "tcp",
			},
		},
		UpdatedAt: &now,
	})

	assert.JSONEq(t, `{
		"app_id":"vaultwarden",
		"state":"needs_attention",
		"message":"container unhealthy",
		"compose_project":"wdm-vaultwarden",
		"stack_path":"/home/test/docker/vaultwarden",
		"needs_attention":true,
		"attention_reasons":["service_unhealthy"],
		"services":[
			{
				"service":"server",
				"state":"running",
				"health":"unhealthy",
				"needs_attention":true,
				"message":"healthcheck failed",
				"published_ports":[{"service":"server","host_ip":"127.0.0.1","host_port":8080,"container_port":80,"protocol":"tcp"}]
			}
		],
		"local_ports":[{"service":"server","host_ip":"127.0.0.1","host_port":8080,"container_port":80,"protocol":"tcp"}],
		"updated_at":"2026-05-29T14:30:00Z"
	}`, got)
}

func TestAppStatus_JSONContract_OmitsZeroUpdatedAt(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.AppStatus{
		AppID:          "vaultwarden",
		State:          "running",
		NeedsAttention: false,
	})

	assert.JSONEq(t, `{
		"app_id":"vaultwarden",
		"state":"running",
		"needs_attention":false
	}`, got)
}

func TestLogLine_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.LogLine{
		Timestamp:      time.Date(2026, time.May, 29, 15, 0, 0, 0, time.UTC),
		AppID:          "vaultwarden",
		ComposeProject: "wdm-vaultwarden",
		ContainerName:  "wdm-vaultwarden-server-1",
		Service:        "server",
		Stream:         "stdout",
		Text:           "started",
	})

	assert.JSONEq(t, `{
		"timestamp":"2026-05-29T15:00:00Z",
		"app_id":"vaultwarden",
		"compose_project":"wdm-vaultwarden",
		"container_name":"wdm-vaultwarden-server-1",
		"service":"server",
		"stream":"stdout",
		"text":"started"
	}`, got)
}

func TestProgressStepConstants_AreStableUniqueAndStringUsable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "StepInstallPlanning", got: types.StepInstallPlanning, want: "step_install_planning"},
		{name: "StepInstallResourceDegraded", got: types.StepInstallResourceDegraded, want: "step_install_resource_degraded"},
		{name: "StepInstallRender", got: types.StepInstallRender, want: "step_install_render"},
		{name: "StepInstallComposeValidate", got: types.StepInstallComposeValidate, want: "step_install_compose_validate"},
		{name: "StepInstallWriteFiles", got: types.StepInstallWriteFiles, want: "step_install_write_files"},
		{name: "StepInstallConfirm", got: types.StepInstallConfirm, want: "step_install_confirm"},
		{name: "StepInstallNetworkCreate", got: types.StepInstallNetworkCreate, want: "step_install_network_create"},
		{name: "StepInstallDeploy", got: types.StepInstallDeploy, want: "step_install_deploy"},
		{name: "StepInstallLockUpdate", got: types.StepInstallLockUpdate, want: "step_install_lock_update"},
		{name: "StepInstallStatus", got: types.StepInstallStatus, want: "step_install_status"},
		{name: "StepUpdatePlanning", got: types.StepUpdatePlanning, want: "step_update_planning"},
		{name: "StepUpdateBackup", got: types.StepUpdateBackup, want: "step_update_backup"},
		{name: "StepUpdateRender", got: types.StepUpdateRender, want: "step_update_render"},
		{name: "StepUpdateComposeValidate", got: types.StepUpdateComposeValidate, want: "step_update_compose_validate"},
		{name: "StepUpdateConfirm", got: types.StepUpdateConfirm, want: "step_update_confirm"},
		{name: "StepUpdatePull", got: types.StepUpdatePull, want: "step_update_pull"},
		{name: "StepUpdateDeploy", got: types.StepUpdateDeploy, want: "step_update_deploy"},
		{name: "StepUpdateLockUpdate", got: types.StepUpdateLockUpdate, want: "step_update_lock_update"},
		{name: "StepUpdateStatus", got: types.StepUpdateStatus, want: "step_update_status"},
		{name: "StepUpdateConfigRestore", got: types.StepUpdateConfigRestore, want: "step_update_config_restore"},
		{name: "StepRemovePlanning", got: types.StepRemovePlanning, want: "step_remove_planning"},
		{name: "StepRemoveConfirm", got: types.StepRemoveConfirm, want: "step_remove_confirm"},
		{name: "StepRemoveComposeDown", got: types.StepRemoveComposeDown, want: "step_remove_compose_down"},
		{name: "StepRemoveLockUpdate", got: types.StepRemoveLockUpdate, want: "step_remove_lock_update"},
		{name: "StepRemoveStatus", got: types.StepRemoveStatus, want: "step_remove_status"},
	}

	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.got)
		})

		previous, exists := seen[tc.got]
		assert.Falsef(t, exists, "duplicate progress step value %q used by %s and %s", tc.got, previous, tc.name)
		seen[tc.got] = tc.name
	}

	fn := types.ProgressFn(func(step string, pct float64, msg string) {
		assert.Equal(t, "step_install_render", step)
		assert.Equal(t, 42.5, pct)
		assert.Equal(t, "rendering", msg)
	})

	// Compile-time/usage guard: step constants must pass to ProgressFn without conversion.
	fn(types.StepInstallRender, 42.5, "rendering")
}

// TestAppRuntimeStatus_JSONContract_PopulatedFields pins the
// `wdm apps list --json` entry shape: the embedded AppInfo facts flatten
// into the same object as the live state and attention reasons, so a
// consumer reads app_id, needs_attention, and state side by side.
func TestAppRuntimeStatus_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.AppRuntimeStatus{
		AppInfo: types.AppInfo{
			AppID:          "uptime-kuma",
			TemplateName:   "Uptime Kuma",
			StackPath:      "/home/test/docker/uptime-kuma",
			CatalogChannel: "stable",
			CatalogVersion: "2026.06.01",
			NeedsAttention: true,
		},
		State:            "needs_attention",
		AttentionReasons: []string{"container_exited"},
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"template_name":"Uptime Kuma",
		"stack_path":"/home/test/docker/uptime-kuma",
		"catalog_channel":"stable",
		"catalog_version":"2026.06.01",
		"last_successful_operation":null,
		"needs_attention":true,
		"state":"needs_attention",
		"attention_reasons":["container_exited"]
	}`, got)
}

// TestAppRuntimeStatus_JSONContract_RunningOmitsAttentionReasons pins that a
// running app reports needs_attention false and omits the empty reasons
// list, so the JSON envelope stays clean for the healthy case.
func TestAppRuntimeStatus_JSONContract_RunningOmitsAttentionReasons(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.AppRuntimeStatus{
		AppInfo: types.AppInfo{AppID: "vaultwarden", StackPath: "/home/test/docker/vaultwarden"},
		State:   "running",
	})

	assert.JSONEq(t, `{
		"app_id":"vaultwarden",
		"template_name":"",
		"stack_path":"/home/test/docker/vaultwarden",
		"catalog_channel":"",
		"catalog_version":"",
		"last_successful_operation":null,
		"needs_attention":false,
		"state":"running"
	}`, got)
}

func mustMarshalJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}
