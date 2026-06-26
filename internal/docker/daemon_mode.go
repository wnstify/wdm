package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wnstify/wdm/pkg/types"
)

// dockerInfoSecurityOptionsFormat projects `docker info` onto its
// SecurityOptions list as JSON. The list carries a "name=rootless" entry
// only when the active daemon runs rootless — the authoritative signal wdm
// uses to refuse a rootful daemon (PRD §11).
const dockerInfoSecurityOptionsFormat = "{{json .SecurityOptions}}"

// IsRootlessDaemon reports whether the active Docker daemon runs rootless,
// detected from `docker info` SecurityOptions carrying a name=rootless entry
// (PRD §11). It fails closed: a nil client or a probe error returns a non-nil
// error, and output that proves nothing (no security options, no rootless
// marker) returns false rather than assuming rootless. Unparseable output is
// a typed [types.ErrCodeDockerUnavailable] error.
func IsRootlessDaemon(ctx context.Context, client Client) (bool, error) {
	if client == nil {
		// Internal programmer error: the only production caller just built the
		// client. A plain error keeps it out of the user-facing typed-code path.
		return false, fmt.Errorf("docker: IsRootlessDaemon requires a non-nil client")
	}

	res, err := client.Run(ctx, InfoInvocation{})
	if err != nil {
		return false, err
	}

	return securityOptionsReportRootless(res.Stdout)
}

// securityOptionsReportRootless parses the JSON `docker info` SecurityOptions
// projection and reports whether it carries the rootless marker. An empty or
// null projection means the daemon reported no security options: that proves
// nothing, so it reads as not-rootless (fail closed). Malformed JSON is a
// typed error so the caller refuses rather than guessing.
func securityOptionsReportRootless(stdout string) (bool, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" || trimmed == "null" {
		return false, nil
	}

	var options []string
	if err := json.Unmarshal([]byte(trimmed), &options); err != nil {
		return false, types.WrapError(
			types.ErrCodeDockerUnavailable,
			"docker daemon security options could not be parsed",
			"verify `docker info` reports its security options and retry",
			fmt.Errorf("parse docker info security options: %w", err),
		)
	}

	for _, option := range options {
		if strings.EqualFold(strings.TrimSpace(option), "name=rootless") {
			return true, nil
		}
	}
	return false, nil
}
