package types_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/wnstify/wdm/pkg/types"
)

// This file pins the trust/distribution JSON shapes: every key is asserted
// snake_case against a fully populated value, omitempty omission is asserted
// for unset optional fields, and the progress Step constants plus the two new
// ConfirmationKind* values are pinned to stable strings.

func TestCatalogUpdateQuery_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.CatalogUpdateQuery{Channel: "stable"})
	assert.JSONEq(t, `{"channel":"stable"}`, got)
}

func TestCatalogUpdateQuery_JSONContract_OmitsEmptyChannel(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.CatalogUpdateQuery{})
	assert.JSONEq(t, `{}`, got)
}

func TestCatalogChange_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.CatalogChange{
		AppID:   "uptime-kuma",
		Kind:    "updated",
		Summary: "image bumped 1.23.0 -> 1.23.1",
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"kind":"updated",
		"summary":"image bumped 1.23.0 -> 1.23.1"
	}`, got)
}

func TestCatalogChange_JSONContract_OmitsEmptySummary(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.CatalogChange{AppID: "freshrss", Kind: "added"})
	assert.JSONEq(t, `{"app_id":"freshrss","kind":"added"}`, got)
}

func TestCatalogUpdateStatus_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	checked := time.Date(2026, time.June, 13, 10, 0, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.CatalogUpdateStatus{
		Channel:         "stable",
		CurrentVersion:  "2026.06.01",
		LatestVersion:   "2026.06.13",
		UpdateAvailable: true,
		Changes: []types.CatalogChange{
			{AppID: "uptime-kuma", Kind: "updated", Summary: "image bumped"},
		},
		Verified:  true,
		CheckedAt: checked,
	})

	assert.JSONEq(t, `{
		"channel":"stable",
		"current_version":"2026.06.01",
		"latest_version":"2026.06.13",
		"update_available":true,
		"changes":[{"app_id":"uptime-kuma","kind":"updated","summary":"image bumped"}],
		"verified":true,
		"checked_at":"2026-06-13T10:00:00Z"
	}`, got)
}

func TestCatalogUpdateStatus_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	// update_available and verified are NOT omitempty: a "no update"
	// or "unverified" result must always serialize as a fail-closed
	// signal, never be inferred from absent fields.
	checked := time.Date(2026, time.June, 13, 10, 0, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.CatalogUpdateStatus{
		Channel:   "stable",
		CheckedAt: checked,
	})

	assert.JSONEq(t, `{
		"channel":"stable",
		"update_available":false,
		"verified":false,
		"checked_at":"2026-06-13T10:00:00Z"
	}`, got)
}

func TestCatalogUpdateRequest_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.CatalogUpdateRequest{
		Channel:       "stable",
		TargetVersion: "2026.06.13",
	})

	assert.JSONEq(t, `{"channel":"stable","target_version":"2026.06.13"}`, got)
}

func TestCatalogUpdateRequest_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.CatalogUpdateRequest{})
	assert.JSONEq(t, `{}`, got)
}

func TestCatalogUpdateResult_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	applied := time.Date(2026, time.June, 13, 10, 5, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.CatalogUpdateResult{
		Channel:            "stable",
		PreviousVersion:    "2026.06.01",
		AppliedVersion:     "2026.06.13",
		VerificationDetail: "checksum, signature, and attestation verified",
		Changes: []types.CatalogChange{
			{AppID: "uptime-kuma", Kind: "updated", Summary: "image bumped"},
		},
		AppliedAt: applied,
	})

	assert.JSONEq(t, `{
		"channel":"stable",
		"previous_version":"2026.06.01",
		"applied_version":"2026.06.13",
		"verification_detail":"checksum, signature, and attestation verified",
		"changes":[{"app_id":"uptime-kuma","kind":"updated","summary":"image bumped"}],
		"applied_at":"2026-06-13T10:05:00Z"
	}`, got)
}

func TestCatalogUpdateResult_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	applied := time.Date(2026, time.June, 13, 10, 5, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.CatalogUpdateResult{
		Channel:        "stable",
		AppliedVersion: "2026.06.13",
		AppliedAt:      applied,
	})

	assert.JSONEq(t, `{
		"channel":"stable",
		"applied_version":"2026.06.13",
		"applied_at":"2026-06-13T10:05:00Z"
	}`, got)
}

func TestSelfUpdateQuery_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	// SelfUpdateQuery is intentionally an empty struct in v1.
	got := mustMarshalJSON(t, types.SelfUpdateQuery{})
	assert.JSONEq(t, `{}`, got)
}

func TestSelfUpdateStatus_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	checked := time.Date(2026, time.June, 13, 11, 0, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.SelfUpdateStatus{
		CurrentVersion:  "0.1.0",
		LatestVersion:   "0.2.0",
		UpdateAvailable: true,
		Verified:        true,
		CheckedAt:       checked,
		Notes:           []string{"executable path is user-writable"},
	})

	assert.JSONEq(t, `{
		"current_version":"0.1.0",
		"latest_version":"0.2.0",
		"update_available":true,
		"verified":true,
		"checked_at":"2026-06-13T11:00:00Z",
		"notes":["executable path is user-writable"]
	}`, got)
}

func TestSelfUpdateStatus_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	// current_version always serializes; update_available and verified
	// are NOT omitempty (the fail-closed signal must always be explicit).
	checked := time.Date(2026, time.June, 13, 11, 0, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.SelfUpdateStatus{
		CurrentVersion: "0.1.0",
		CheckedAt:      checked,
	})

	assert.JSONEq(t, `{
		"current_version":"0.1.0",
		"update_available":false,
		"verified":false,
		"checked_at":"2026-06-13T11:00:00Z"
	}`, got)
}

func TestSelfUpdateRequest_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.SelfUpdateRequest{TargetVersion: "0.2.0"})
	assert.JSONEq(t, `{"target_version":"0.2.0"}`, got)
}

func TestSelfUpdateRequest_JSONContract_OmitsEmptyTargetVersion(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.SelfUpdateRequest{})
	assert.JSONEq(t, `{}`, got)
}

func TestSelfUpdateResult_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.SelfUpdateResult{
		PreviousVersion:    "0.1.0",
		AppliedVersion:     "0.2.0",
		Replaced:           true,
		SmokeOK:            true,
		RolledBack:         false,
		PreviousBinaryPath: "/home/test/.local/bin/wdm.previous",
		Message:            "updated to 0.2.0",
	})

	assert.JSONEq(t, `{
		"previous_version":"0.1.0",
		"applied_version":"0.2.0",
		"replaced":true,
		"smoke_ok":true,
		"rolled_back":false,
		"previous_binary_path":"/home/test/.local/bin/wdm.previous",
		"message":"updated to 0.2.0"
	}`, got)
}

func TestSelfUpdateResult_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	// previous_version, replaced, smoke_ok, and rolled_back are NOT
	// omitempty: the outcome booleans must always serialize so a
	// failed-smoke rollback (rolled_back:true) is explicit.
	got := mustMarshalJSON(t, types.SelfUpdateResult{PreviousVersion: "0.1.0"})

	assert.JSONEq(t, `{
		"previous_version":"0.1.0",
		"replaced":false,
		"smoke_ok":false,
		"rolled_back":false
	}`, got)
}

func TestImageUpdateQuery_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	got := mustMarshalJSON(t, types.ImageUpdateQuery{AppID: "uptime-kuma"})
	assert.JSONEq(t, `{"app_id":"uptime-kuma"}`, got)
}

func TestImageUpdateReport_JSONContract_PopulatedFields(t *testing.T) {
	t.Parallel()

	checked := time.Date(2026, time.June, 13, 12, 0, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.ImageUpdateReport{
		AppID: "uptime-kuma",
		Candidates: []types.ImageUpdateCandidate{
			{
				Service:         "app",
				Image:           "louislam/uptime-kuma",
				CurrentTag:      "1.23.0",
				CurrentDigest:   "sha256:aaa",
				LatestTag:       "1.23.1",
				LatestDigest:    "sha256:bbb",
				UpdateAvailable: true,
			},
		},
		CheckedAt: checked,
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"candidates":[{
			"service":"app",
			"image":"louislam/uptime-kuma",
			"current_tag":"1.23.0",
			"current_digest":"sha256:aaa",
			"latest_tag":"1.23.1",
			"latest_digest":"sha256:bbb",
			"update_available":true
		}],
		"checked_at":"2026-06-13T12:00:00Z"
	}`, got)
}

func TestImageUpdateReport_JSONContract_OmitsEmptyCandidates(t *testing.T) {
	t.Parallel()

	checked := time.Date(2026, time.June, 13, 12, 0, 0, 0, time.UTC)
	got := mustMarshalJSON(t, types.ImageUpdateReport{
		AppID:     "uptime-kuma",
		CheckedAt: checked,
	})

	assert.JSONEq(t, `{
		"app_id":"uptime-kuma",
		"checked_at":"2026-06-13T12:00:00Z"
	}`, got)
}

func TestImageUpdateCandidate_JSONContract_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	// update_available is NOT omitempty: a "no change" finding must
	// always serialize. The current/latest tag and digest fields omit on
	// the zero value (digests are surfaced when known and omitted
	// otherwise).
	got := mustMarshalJSON(t, types.ImageUpdateCandidate{
		Service: "app",
		Image:   "louislam/uptime-kuma",
	})

	assert.JSONEq(t, `{
		"service":"app",
		"image":"louislam/uptime-kuma",
		"update_available":false
	}`, got)
}

func TestConfirmationKinds_AreStable(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "catalog_update", types.ConfirmationKindCatalogUpdate)
	assert.Equal(t, "self_update", types.ConfirmationKindSelfUpdate)
}

func TestProgressStepConstants_CatalogAndSelfUpdateAreStableAndUnique(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "StepCatalogUpdatePlanning", got: types.StepCatalogUpdatePlanning, want: "step_catalog_update_planning"},
		{name: "StepCatalogUpdateDownload", got: types.StepCatalogUpdateDownload, want: "step_catalog_update_download"},
		{name: "StepCatalogUpdateVerify", got: types.StepCatalogUpdateVerify, want: "step_catalog_update_verify"},
		{name: "StepCatalogUpdateConfirm", got: types.StepCatalogUpdateConfirm, want: "step_catalog_update_confirm"},
		{name: "StepCatalogUpdateApply", got: types.StepCatalogUpdateApply, want: "step_catalog_update_apply"},
		{name: "StepCatalogUpdateStatus", got: types.StepCatalogUpdateStatus, want: "step_catalog_update_status"},
		{name: "StepSelfUpdatePlanning", got: types.StepSelfUpdatePlanning, want: "step_self_update_planning"},
		{name: "StepSelfUpdateDownload", got: types.StepSelfUpdateDownload, want: "step_self_update_download"},
		{name: "StepSelfUpdateVerify", got: types.StepSelfUpdateVerify, want: "step_self_update_verify"},
		{name: "StepSelfUpdateConfirm", got: types.StepSelfUpdateConfirm, want: "step_self_update_confirm"},
		{name: "StepSelfUpdateStage", got: types.StepSelfUpdateStage, want: "step_self_update_stage"},
		{name: "StepSelfUpdateReplace", got: types.StepSelfUpdateReplace, want: "step_self_update_replace"},
		{name: "StepSelfUpdateSmoke", got: types.StepSelfUpdateSmoke, want: "step_self_update_smoke"},
		{name: "StepSelfUpdateRollback", got: types.StepSelfUpdateRollback, want: "step_self_update_rollback"},
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
