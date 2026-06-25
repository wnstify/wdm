package core

import (
	"fmt"
	"strings"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

// generatedCredentialNote is the verbatim line shown beside every
// one-time credential on the finish screen. The plaintext cannot be
// re-derived from the persisted PHC hash, so the operator must record it
// at install time.
const generatedCredentialNote = "Store this now — it cannot be recovered."

// generateSecretsAndBindRedactor mints the install's secrets and returns a
// redactor bound to the resulting generated-secret set in one step, so the
// redactor is never constructed before generation completes (issue #120).
// generateInstallSecrets stays the single atomic generation step; this only
// orders generation and binding inseparably.
func (p *installPlan) generateSecretsAndBindRedactor(
	generate func(security.Encoding) (string, error),
	generateArgon2id func() (plaintext, phc string, err error),
) (security.Redactor, error) {
	if err := p.generateInstallSecrets(generate, generateArgon2id); err != nil {
		return nil, err
	}
	return security.NewActiveRedactor(p.generatedValues), nil
}

func (p *installPlan) generateInstallSecrets(
	generate func(security.Encoding) (string, error),
	generateArgon2id func() (plaintext, phc string, err error),
) error {
	if generate == nil || generateArgon2id == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"secret generator is required",
			"construct the engine with a secret generator",
		)
	}
	for _, ph := range p.app.Placeholders {
		if render.Type(ph.Type) != render.TypeSecret {
			continue
		}
		encoding := security.Encoding(ph.Encoding)
		switch encoding {
		case security.EncodingBase64URL, security.EncodingBase64Std, security.EncodingHex:
			value, err := generate(encoding)
			if err != nil {
				return err
			}
			p.resolvedValues[ph.Name] = value
			p.generatedValues = append(p.generatedValues, value)
		case security.EncodingArgon2id:
			if err := p.generateArgon2idSecret(ph, generateArgon2id); err != nil {
				return err
			}
		default:
			return catalogVerificationError(
				"catalog contains an invalid secret encoding",
				"refresh the catalog and retry",
				fmt.Errorf("placeholder %q has encoding %q", ph.Name, ph.Encoding),
			)
		}
	}
	return nil
}

// generateArgon2idSecret mints a one-time argon2id credential for ph and
// binds it across the three sinks with the right value in each:
//   - the .env (and the redaction set) get the $$-escaped PHC hash, so
//     Compose's --env-file interpolation collapses each $$ back to a single
//     $ and the container sees the canonical PHC (see [installEnvEscapeDollar]);
//   - the one-time plaintext goes ONLY to [installPlan.shownCredentials],
//     never to resolvedValues, generatedValues, or any persisted artifact.
//
// The escaping is scoped to this encoding alone — base64url/hex values
// never contain `$` and must pass through the value-agnostic renderer
// untouched, so it lives here at bind time, not in [render.RenderEnv].
func (p *installPlan) generateArgon2idSecret(
	ph catalog.Placeholder,
	generateArgon2id func() (plaintext, phc string, err error),
) error {
	plaintext, phc, err := generateArgon2id()
	if err != nil {
		return err
	}
	escapedPHC := installEnvEscapeDollar(phc)
	p.resolvedValues[ph.Name] = escapedPHC
	p.generatedValues = append(p.generatedValues, escapedPHC)
	p.shownCredentials = append(p.shownCredentials, types.GeneratedCredential{
		Label: strings.TrimSpace(p.app.Name + " " + ph.Name),
		Value: plaintext,
		Note:  generatedCredentialNote,
	})
	return nil
}

// installRedactionSecrets is the full secret-aware redaction set for an
// install's logger: the persisted generated values plus every argon2id
// one-time plaintext (which lives only in shownCredentials, never in
// generatedValues). Clones rather than mutating generatedValues so the
// plan's persisted secret set stays unchanged (PRD §24 rule 2).
func installRedactionSecrets(plan *installPlan) []string {
	secrets := make([]string, 0, len(plan.generatedValues)+len(plan.shownCredentials))
	secrets = append(secrets, plan.generatedValues...)
	for _, cred := range plan.shownCredentials {
		secrets = append(secrets, cred.Value)
	}
	return append(secrets, sensitiveSetValues(plan)...)
}

// sensitiveSetValues returns the resolved plaintext of user-supplied
// placeholders flagged Sensitive (type:string via --set). wdm never
// generates these, so they are not secret-typed; they are collected
// separately for value-redaction and non-secret leak-checking, matching
// the reused-secret treatment on the update and reconfigure paths.
func sensitiveSetValues(plan *installPlan) []string {
	var vals []string
	for _, ph := range plan.app.Placeholders {
		if ph.Sensitive {
			if v := plan.resolvedValues[ph.Name]; v != "" {
				vals = append(vals, v)
			}
		}
	}
	return vals
}

// installEnvEscapeDollar doubles every `$` so a value survives Docker
// Compose's --env-file variable interpolation verbatim. Compose expands
// `$VAR` / `${VAR}` references read from the env-file and treats `$$` as a
// literal `$`; an un-escaped argon2id PHC (`$argon2id$v=19$m=...`) would
// otherwise have its segments interpreted as variable references and
// corrupted before the container sees them.
func installEnvEscapeDollar(value string) string {
	return strings.ReplaceAll(value, "$", "$$")
}
