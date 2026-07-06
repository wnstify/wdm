package types_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/wnstify/wdm/pkg/types"
)

// This file pins the engine-gap JSON shapes: every key is asserted
// snake_case against a fully populated value, omitempty omission is
// asserted for unset optional fields, and the progress Step constants
// plus ConfirmationKindDeleteDestructive are pinned to stable strings.

func TestRestartRequest_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.RestartRequest{
		AppID:     "uptime-kuma",
		StackPath: "/home/test/docker/uptime-kuma",
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"stack_path":"/home/test/docker/uptime-kuma"
	}`, got)
}

func TestRestartRequest_JSONContract_OmitsEmptyStackPath(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.RestartRequest{AppID: "uptime-kuma"})

	assert.JSONEq(t, `{"app_id":"uptime-kuma"}`, got)
}

func TestRestartResult_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.RestartResult{
		AppID:             "uptime-kuma",
		ComposeProject:    "wdm-uptime-kuma",
		RestartedServices: []string{"app", "db"},
		Status: &types.AppStatus{
			AppID:          "uptime-kuma",
			State:          "running",
			NeedsAttention: false,
		},
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"compose_project":"wdm-uptime-kuma",
		"restarted_services":["app","db"],
		"status":{"app_id":"uptime-kuma","state":"running","needs_attention":false}
	}`, got)
}

func TestRestartResult_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.RestartResult{AppID: "uptime-kuma"})

	assert.JSONEq(t, `{"app_id":"uptime-kuma"}`, got)
}

func TestStopAllResult_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.StopAllResult{
		Stopped: []types.StoppedApp{
			{AppID: "uptime-kuma", ComposeProject: "wdm-uptime-kuma"},
		},
		Failed: []types.StoppedApp{
			{AppID: "freshrss", ComposeProject: "wdm-freshrss", Error: "daemon unreachable"},
		},
		AlreadyStopped: []types.StoppedApp{
			{AppID: "vaultwarden"},
		},
	})

	assert.JSONEq(t, `{
		"stopped":[{"app_id":"uptime-kuma","compose_project":"wdm-uptime-kuma"}],
		"failed":[{"app_id":"freshrss","compose_project":"wdm-freshrss","error":"daemon unreachable"}],
		"already_stopped":[{"app_id":"vaultwarden"}]
	}`, got)
}

func TestStopAllResult_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.StopAllResult{})

	assert.JSONEq(t, `{}`, got)
}

func TestValidationResult_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.ValidationResult{
		AppID:          "uptime-kuma",
		ComposeProject: "wdm-uptime-kuma",
		ComposeFile:    "/home/test/docker/uptime-kuma/docker-compose.yml",
		Valid:          false,
		Detail:         "service app: missing image",
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"compose_project":"wdm-uptime-kuma",
		"compose_file":"/home/test/docker/uptime-kuma/docker-compose.yml",
		"valid":false,
		"detail":"service app: missing image"
	}`, got)
}

func TestValidationResult_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	// Valid is NOT omitempty: a true/false result must always serialize.
	got := mustMarshalJSON(t, types.ValidationResult{
		AppID: "uptime-kuma",
		Valid: true,
	})

	assert.JSONEq(t, `{"app_id":"uptime-kuma","valid":true}`, got)
}

func TestBackupInfo_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, time.June, 1, 9, 0, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.BackupInfo{
		SnapshotID: "1717000000000000000-update",
		Operation:  "update",
		CreatedAt:  created,
		Path:       "/home/test/docker/uptime-kuma/.wdm-backups/1717000000000000000-update",
		Files:      []string{"docker-compose.yml", ".env", ".wdm.lock"},
	})

	assert.JSONEq(t, `{
		"snapshot_id":"1717000000000000000-update",
		"operation":"update",
		"created_at":"2026-06-01T09:00:00Z",
		"path":"/home/test/docker/uptime-kuma/.wdm-backups/1717000000000000000-update",
		"files":["docker-compose.yml",".env",".wdm.lock"]
	}`, got)
}

func TestBackupInfo_JSONContract_OmitsEmptyFiles(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, time.June, 1, 9, 0, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.BackupInfo{
		SnapshotID: "1717000000000000000-install",
		Operation:  "install",
		CreatedAt:  created,
		Path:       "/home/test/docker/uptime-kuma/.wdm-backups/1717000000000000000-install",
	})

	assert.JSONEq(t, `{
		"snapshot_id":"1717000000000000000-install",
		"operation":"install",
		"created_at":"2026-06-01T09:00:00Z",
		"path":"/home/test/docker/uptime-kuma/.wdm-backups/1717000000000000000-install"
	}`, got)
}

func TestRestoreBackupRequest_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.RestoreBackupRequest{
		AppID:      "uptime-kuma",
		StackPath:  "/home/test/docker/uptime-kuma",
		SnapshotID: "1717000000000000000-update",
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"stack_path":"/home/test/docker/uptime-kuma",
		"snapshot_id":"1717000000000000000-update"
	}`, got)
}

func TestRestoreBackupRequest_JSONContract_OmitsEmptyStackPath(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.RestoreBackupRequest{
		AppID:      "uptime-kuma",
		SnapshotID: "1717000000000000000-update",
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"snapshot_id":"1717000000000000000-update"
	}`, got)
}

func TestRestoreBackupResult_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.RestoreBackupResult{
		AppID:          "uptime-kuma",
		SnapshotID:     "1717000000000000000-update",
		RestoredFiles:  []string{"docker-compose.yml", ".env"},
		BoundaryNotice: "Config files were restored. App data, databases, and volumes were not.",
		NextAction:     "apply the update to recreate containers with the restored config",
		Status: &types.AppStatus{
			AppID:          "uptime-kuma",
			State:          "running",
			NeedsAttention: false,
		},
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"snapshot_id":"1717000000000000000-update",
		"restored_files":["docker-compose.yml",".env"],
		"boundary_notice":"Config files were restored. App data, databases, and volumes were not.",
		"next_action":"apply the update to recreate containers with the restored config",
		"status":{"app_id":"uptime-kuma","state":"running","needs_attention":false}
	}`, got)
}

func TestRestoreBackupResult_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	// boundary_notice is NOT omitempty: the config-only guarantee must
	// always serialize, even as an empty string on a zero value.
	got := mustMarshalJSON(t, types.RestoreBackupResult{
		AppID:      "uptime-kuma",
		SnapshotID: "1717000000000000000-update",
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"snapshot_id":"1717000000000000000-update",
		"boundary_notice":""
	}`, got)
}

func TestCatalogQuery_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.CatalogQuery{Channel: "stable"})
	assert.JSONEq(t, `{"channel":"stable"}`, got)
}

func TestCatalogQuery_JSONContract_OmitsEmptyChannel(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.CatalogQuery{})
	assert.JSONEq(t, `{}`, got)
}

func TestCatalogAppQuery_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.CatalogAppQuery{
		AppID:   "uptime-kuma",
		Channel: "stable",
	})

	assert.JSONEq(t, `{"app_id":"uptime-kuma","channel":"stable"}`, got)
}

func TestCatalogApp_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.CatalogApp{
		AppID:           "uptime-kuma",
		Name:            "Uptime Kuma",
		Summary:         "Self-hosted monitoring tool",
		Description:     "A fancy self-hosted monitoring tool.",
		TemplateName:    "uptime-kuma",
		TemplateVersion: "2026-06-11",
		Channel:         "stable",
		Placeholders: []types.CatalogPlaceholder{
			{
				Key:         "DB_PASSWORD",
				Type:        "secret",
				Required:    true,
				Secret:      true,
				Description: "database password",
				Default:     "",
				Pattern:     "",
			},
			{
				Key:         "DOMAIN",
				Type:        "domain",
				Required:    true,
				Secret:      false,
				Description: "public domain",
				Default:     "example.com",
				Pattern:     "^[a-z0-9.-]+$",
			},
		},
		Ports: []types.CatalogPort{
			{Service: "app", Host: 3008, Container: 3001, Protocol: "tcp"},
		},
		ImagePins: []types.CatalogImagePin{
			{Service: "app", Image: "louislam/uptime-kuma", Tag: "1.23.0"},
		},
		Resources: []types.CatalogResource{
			{Service: "app", MemoryRecommended: "512m", CPUsRecommended: "1.0"},
		},
		RiskClassification: []string{"database"},
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"name":"Uptime Kuma",
		"summary":"Self-hosted monitoring tool",
		"description":"A fancy self-hosted monitoring tool.",
		"template_name":"uptime-kuma",
		"template_version":"2026-06-11",
		"channel":"stable",
		"placeholders":[
			{"key":"DB_PASSWORD","type":"secret","required":true,"secret":true,"description":"database password"},
			{"key":"DOMAIN","type":"domain","required":true,"secret":false,"description":"public domain","default":"example.com","pattern":"^[a-z0-9.-]+$"}
		],
		"ports":[{"service":"app","host":3008,"container":3001,"protocol":"tcp"}],
		"image_pins":[{"service":"app","image":"louislam/uptime-kuma","tag":"1.23.0"}],
		"resources":[{"service":"app","memory_recommended":"512m","cpus_recommended":"1.0"}],
		"risk_classification":["database"]
	}`, got)
}

func TestCatalogApp_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.CatalogApp{
		AppID:           "uptime-kuma",
		Name:            "Uptime Kuma",
		Summary:         "Self-hosted monitoring tool",
		TemplateName:    "uptime-kuma",
		TemplateVersion: "2026-06-11",
		Channel:         "stable",
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"name":"Uptime Kuma",
		"summary":"Self-hosted monitoring tool",
		"template_name":"uptime-kuma",
		"template_version":"2026-06-11",
		"channel":"stable"
	}`, got)
}

func TestDeleteRequest_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.DeleteRequest{
		AppID:              "uptime-kuma",
		StackPath:          "/home/test/docker/uptime-kuma",
		ConfirmationName:   "uptime-kuma",
		DeleteNamedVolumes: true,
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"stack_path":"/home/test/docker/uptime-kuma",
		"confirmation_name":"uptime-kuma",
		"delete_named_volumes":true
	}`, got)
}

func TestDeleteRequest_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	// confirmation_name is NOT omitempty (the engine re-verifies it);
	// stack_path and delete_named_volumes are omitempty.
	got := mustMarshalJSON(t, types.DeleteRequest{
		AppID:            "uptime-kuma",
		ConfirmationName: "uptime-kuma",
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"confirmation_name":"uptime-kuma"
	}`, got)
}

func TestDeleteResult_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.DeleteResult{
		AppID:                 "uptime-kuma",
		DeletedPaths:          []string{"/home/test/docker/uptime-kuma"},
		RemainingNamedVolumes: []string{"wdm-uptime-kuma_db_data"},
		RemovedNetworks:       []string{"kuma"},
		RetainedNetworks: []types.RetainedNetwork{
			{Name: "shared", Reason: "network shared has active endpoints"},
		},
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"deleted_paths":["/home/test/docker/uptime-kuma"],
		"remaining_named_volumes":["wdm-uptime-kuma_db_data"],
		"removed_networks":["kuma"],
		"retained_networks":[{"name":"shared","reason":"network shared has active endpoints"}]
	}`, got)
}

func TestDeleteResult_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.DeleteResult{AppID: "uptime-kuma"})
	assert.JSONEq(t, `{"app_id":"uptime-kuma"}`, got)
}

func TestRuntimeLockStatus_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, time.June, 1, 8, 0, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.RuntimeLockStatus{
		Exists:        true,
		Held:          true,
		Stale:         true,
		HolderPID:     4242,
		HolderCommand: "install",
		HolderAlive:   false,
		StartedAt:     &started,
		WDMVersion:    "0.1.0",
	})

	assert.JSONEq(t, `{
		"exists":true,
		"held":true,
		"stale":true,
		"holder_pid":4242,
		"holder_command":"install",
		"holder_alive":false,
		"started_at":"2026-06-01T08:00:00Z",
		"wdm_version":"0.1.0"
	}`, got)
}

func TestRuntimeLockStatus_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	// exists/held/stale and holder_alive are NOT omitempty: all four
	// lock-state booleans must always serialize so a dead holder
	// (holder_alive:false alongside holder metadata) is distinguishable
	// from no recorded holder (false with the holder fields absent). The
	// remaining holder fields (holder_pid/holder_command/started_at/
	// wdm_version) omit on the zero value. Re-adding omitempty to
	// HolderAlive would drop holder_alive from this payload and fail here.
	got := mustMarshalJSON(t, types.RuntimeLockStatus{
		Exists: false,
		Held:   false,
		Stale:  false,
	})

	assert.JSONEq(t, `{
		"exists":false,
		"held":false,
		"stale":false,
		"holder_alive":false
	}`, got)
}

func TestConfirmationKindDeleteDestructive_IsStable(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "delete_destructive", types.ConfirmationKindDeleteDestructive)
}

func TestConfirmationKindUninstallDestructive_IsStable(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "uninstall_destructive", types.ConfirmationKindUninstallDestructive)
}

func TestProgressStepConstants_UninstallAreStableAndUnique(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "StepUninstallPlanning", got: types.StepUninstallPlanning, want: "step_uninstall_planning"},
		{name: "StepUninstallConfirm", got: types.StepUninstallConfirm, want: "step_uninstall_confirm"},
		{name: "StepUninstallTeardown", got: types.StepUninstallTeardown, want: "step_uninstall_teardown"},
		{name: "StepUninstallRemoveFootprint", got: types.StepUninstallRemoveFootprint, want: "step_uninstall_remove_footprint"},
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
}

func TestProgressStepConstants_RestartRestoreDeleteAreStableAndUnique(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "StepRestartPlanning", got: types.StepRestartPlanning, want: "step_restart_planning"},
		{name: "StepRestartConfirm", got: types.StepRestartConfirm, want: "step_restart_confirm"},
		{name: "StepRestartExecute", got: types.StepRestartExecute, want: "step_restart_execute"},
		{name: "StepRestartStatus", got: types.StepRestartStatus, want: "step_restart_status"},
		{name: "StepRedeployPlanning", got: types.StepRedeployPlanning, want: "step_redeploy_planning"},
		{name: "StepRedeployConfirm", got: types.StepRedeployConfirm, want: "step_redeploy_confirm"},
		{name: "StepRedeployExecute", got: types.StepRedeployExecute, want: "step_redeploy_execute"},
		{name: "StepRedeployStatus", got: types.StepRedeployStatus, want: "step_redeploy_status"},
		{name: "StepStopAllPlanning", got: types.StepStopAllPlanning, want: "step_stop_all_planning"},
		{name: "StepStopAllConfirm", got: types.StepStopAllConfirm, want: "step_stop_all_confirm"},
		{name: "StepStopAllExecute", got: types.StepStopAllExecute, want: "step_stop_all_execute"},
		{name: "StepRestorePlanning", got: types.StepRestorePlanning, want: "step_restore_planning"},
		{name: "StepRestoreConfirm", got: types.StepRestoreConfirm, want: "step_restore_confirm"},
		{name: "StepRestoreExecute", got: types.StepRestoreExecute, want: "step_restore_execute"},
		{name: "StepRestoreStatus", got: types.StepRestoreStatus, want: "step_restore_status"},
		{name: "StepDeletePlanning", got: types.StepDeletePlanning, want: "step_delete_planning"},
		{name: "StepDeleteConfirm", got: types.StepDeleteConfirm, want: "step_delete_confirm"},
		{name: "StepDeleteHelperPull", got: types.StepDeleteHelperPull, want: "step_delete_helper_pull"},
		{name: "StepDeleteComposeDown", got: types.StepDeleteComposeDown, want: "step_delete_compose_down"},
		{name: "StepDeleteRemoveNetworks", got: types.StepDeleteRemoveNetworks, want: "step_delete_remove_networks"},
		{name: "StepDeleteFiles", got: types.StepDeleteFiles, want: "step_delete_files"},
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
}
