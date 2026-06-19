package types

const (
	// StepInstallPlanning reports install planning.
	StepInstallPlanning = "step_install_planning"
	// StepInstallResourceDegraded reports fallback from recommended sizing.
	StepInstallResourceDegraded = "step_install_resource_degraded"
	// StepInstallRender reports template rendering.
	StepInstallRender = "step_install_render"
	// StepInstallComposeValidate reports compose config validation.
	StepInstallComposeValidate = "step_install_compose_validate"
	// StepInstallWriteFiles reports stack file writes.
	StepInstallWriteFiles = "step_install_write_files"
	// StepInstallConfirm reports user confirmation gating.
	StepInstallConfirm = "step_install_confirm"
	// StepInstallNetworkCreate reports catalog network pre-creation.
	StepInstallNetworkCreate = "step_install_network_create"
	// StepInstallDeploy reports docker compose deployment.
	StepInstallDeploy = "step_install_deploy"
	// StepInstallLockUpdate reports.wdm.lock update.
	StepInstallLockUpdate = "step_install_lock_update"
	// StepInstallStatus reports post-install status verification.
	StepInstallStatus = "step_install_status"

	// StepUpdatePlanning reports update planning.
	StepUpdatePlanning = "step_update_planning"
	// StepUpdateBackup reports pre-update backup creation.
	StepUpdateBackup = "step_update_backup"
	// StepUpdateRender reports update-time rendering.
	StepUpdateRender = "step_update_render"
	// StepUpdateComposeValidate reports compose validation for update.
	StepUpdateComposeValidate = "step_update_compose_validate"
	// StepUpdateConfirm reports update confirmation gating.
	StepUpdateConfirm = "step_update_confirm"
	// StepUpdatePull reports image pull operations.
	StepUpdatePull = "step_update_pull"
	// StepUpdateDeploy reports update deployment.
	StepUpdateDeploy = "step_update_deploy"
	// StepUpdateLockUpdate reports lock manifest update after deploy.
	StepUpdateLockUpdate = "step_update_lock_update"
	// StepUpdateStatus reports post-update status verification.
	StepUpdateStatus = "step_update_status"
	// StepUpdateConfigRestore reports update-time config restore.
	StepUpdateConfigRestore = "step_update_config_restore"

	// StepReconfigurePlanning reports reconfigure planning.
	StepReconfigurePlanning = "step_reconfigure_planning"
	// StepReconfigureBackup reports pre-reconfigure backup creation.
	StepReconfigureBackup = "step_reconfigure_backup"
	// StepReconfigureRender reports reconfigure-time rendering.
	StepReconfigureRender = "step_reconfigure_render"
	// StepReconfigureComposeValidate reports compose validation for reconfigure.
	StepReconfigureComposeValidate = "step_reconfigure_compose_validate"
	// StepReconfigureConfirm reports reconfigure confirmation gating.
	StepReconfigureConfirm = "step_reconfigure_confirm"
	// StepReconfigureDeploy reports reconfigure deployment.
	StepReconfigureDeploy = "step_reconfigure_deploy"
	// StepReconfigureLockUpdate reports lock manifest update after deploy.
	StepReconfigureLockUpdate = "step_reconfigure_lock_update"
	// StepReconfigureStatus reports post-reconfigure status verification.
	StepReconfigureStatus = "step_reconfigure_status"
	// StepReconfigureConfigRestore reports reconfigure-time config restore.
	StepReconfigureConfigRestore = "step_reconfigure_config_restore"

	// StepRemovePlanning reports remove planning.
	StepRemovePlanning = "step_remove_planning"
	// StepRemoveConfirm reports remove confirmation gating.
	StepRemoveConfirm = "step_remove_confirm"
	// StepRemoveComposeDown reports docker compose down execution.
	StepRemoveComposeDown = "step_remove_compose_down"
	// StepRemoveLockUpdate reports lock update/removal during remove.
	StepRemoveLockUpdate = "step_remove_lock_update"
	// StepRemoveStatus reports post-remove status verification.
	StepRemoveStatus = "step_remove_status"

	// StepRestartPlanning reports restart planning.
	StepRestartPlanning = "step_restart_planning"
	// StepRestartConfirm reports restart confirmation gating.
	StepRestartConfirm = "step_restart_confirm"
	// StepRestartExecute reports docker compose restart execution.
	StepRestartExecute = "step_restart_execute"
	// StepRestartStatus reports post-restart status verification.
	StepRestartStatus = "step_restart_status"

	// StepStopAllPlanning reports stop-all managed-stack enumeration.
	StepStopAllPlanning = "step_stop_all_planning"
	// StepStopAllConfirm reports stop-all confirmation gating.
	StepStopAllConfirm = "step_stop_all_confirm"
	// StepStopAllExecute reports per-stack docker compose stop execution.
	StepStopAllExecute = "step_stop_all_execute"

	// StepUninstallPlanning reports self-uninstall managed-stack enumeration.
	StepUninstallPlanning = "step_uninstall_planning"
	// StepUninstallConfirm reports self-uninstall confirmation gating.
	StepUninstallConfirm = "step_uninstall_confirm"
	// StepUninstallTeardown reports per-stack docker compose down --rmi all teardown.
	StepUninstallTeardown = "step_uninstall_teardown"
	// StepUninstallNetworkSweep reports the label-based sweep of every remaining
	// wdm.managed=true network, including orphaned ones, after teardown.
	StepUninstallNetworkSweep = "step_uninstall_network_sweep"
	// StepUninstallRemoveFootprint reports removal of wdm's on-disk footprint.
	StepUninstallRemoveFootprint = "step_uninstall_remove_footprint"

	// StepRestorePlanning reports config-restore planning.
	StepRestorePlanning = "step_restore_planning"
	// StepRestoreConfirm reports config-restore confirmation gating.
	StepRestoreConfirm = "step_restore_confirm"
	// StepRestoreExecute reports config-restore file rewrite.
	StepRestoreExecute = "step_restore_execute"
	// StepRestoreStatus reports post-restore status verification.
	StepRestoreStatus = "step_restore_status"

	// StepDeletePlanning reports destructive-delete planning.
	StepDeletePlanning = "step_delete_planning"
	// StepDeleteConfirm reports destructive-delete confirmation gating.
	StepDeleteConfirm = "step_delete_confirm"
	// StepDeleteComposeDown reports docker compose down during delete.
	StepDeleteComposeDown = "step_delete_compose_down"
	// StepDeleteRemoveNetworks reports best-effort removal of the app's
	// wdm-created networks after the down and before stack-file deletion.
	StepDeleteRemoveNetworks = "step_delete_remove_networks"
	// StepDeleteFiles reports stack-file deletion.
	StepDeleteFiles = "step_delete_files"

	// StepCatalogUpdatePlanning reports catalog-update planning.
	StepCatalogUpdatePlanning = "step_catalog_update_planning"
	// StepCatalogUpdateDownload reports catalog artifact download.
	StepCatalogUpdateDownload = "step_catalog_update_download"
	// StepCatalogUpdateVerify reports catalog checksum/signature/attestation verification.
	StepCatalogUpdateVerify = "step_catalog_update_verify"
	// StepCatalogUpdateConfirm reports catalog-update confirmation gating.
	StepCatalogUpdateConfirm = "step_catalog_update_confirm"
	// StepCatalogUpdateApply reports the verified catalog write.
	StepCatalogUpdateApply = "step_catalog_update_apply"
	// StepCatalogUpdateStatus reports post-apply catalog status verification.
	StepCatalogUpdateStatus = "step_catalog_update_status"

	// StepSelfUpdatePlanning reports binary self-update planning.
	StepSelfUpdatePlanning = "step_self_update_planning"
	// StepSelfUpdateDownload reports candidate binary download.
	StepSelfUpdateDownload = "step_self_update_download"
	// StepSelfUpdateVerify reports candidate checksum/signature/attestation verification.
	StepSelfUpdateVerify = "step_self_update_verify"
	// StepSelfUpdateConfirm reports self-update confirmation gating.
	StepSelfUpdateConfirm = "step_self_update_confirm"
	// StepSelfUpdateStage reports staging of the verified candidate binary.
	StepSelfUpdateStage = "step_self_update_stage"
	// StepSelfUpdateReplace reports atomic replacement of the binary.
	StepSelfUpdateReplace = "step_self_update_replace"
	// StepSelfUpdateSmoke reports the post-replace `wdm --version` smoke check.
	StepSelfUpdateSmoke = "step_self_update_smoke"
	// StepSelfUpdateRollback reports restoration of the previous binary.
	StepSelfUpdateRollback = "step_self_update_rollback"
)

// Progress is the JSON-serializable equivalent of the three arguments
// passed to ProgressFn (PRD §37) — the on-the-wire shape used when the
// future GUI streams progress events over IPC. The in-process callback
// in pkg/engine instead takes the three values directly, matching the
// locked load-bearing signature in the invariant:
// func(step string, pct float64, msg string).
type Progress struct {
	// Step is a short identifier for the current pipeline stage —
	// e.g. "render_compose" or "pull_images". Stable across releases
	// so logs and dashboards can group by Step safely.
	Step string `json:"step"`

	// Percent is a coarse completion estimate in [0.0, 100.0]. May be
	// approximate; the engine sets it to 100 only on terminal events.
	Percent float64 `json:"percent"`

	// Message is the human-readable status line shown alongside Step.
	Message string `json:"message"`
}
