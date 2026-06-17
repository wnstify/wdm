package system

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
)

// Identity is a snapshot of the invoking user, captured once at startup
// and threaded through later checks. Capturing it once keeps every check
// deterministic for the process lifetime and avoids a uid lookup on each
// log line or state write.
// All fields are populated by [CurrentIdentity]. Callers MUST NOT build
// an Identity by hand: the zero value would mis-trip downstream checks
// that key on EUID.
type Identity struct {
	// EUID is the effective UID when [CurrentIdentity] ran. PRD §11's
	// refusal gate keys on the effective UID, not the real UID, so a
	// setuid binary that dropped privileges still gets the right decision.
	EUID int

	// GID is the primary group ID, parsed from [user.User.Gid].
	GID int

	// GroupIDs is the user's full group membership. [CurrentIdentity]
	// always includes GID, even when the OS lookup omits the primary
	// group.
	GroupIDs []int

	// Username is the OS account name (e.g. "alice"). Never empty for an
	// Identity returned by [CurrentIdentity] without error.
	Username string

	// Home is the absolute path to the user's home directory. PRD §13
	// anchors wdm's XDG-clean layout under it.
	Home string
}

// CurrentIdentity captures the invoking user's identity from
// [os.Geteuid] and [user.Current]. Errors are wrapped with the
// "system.CurrentIdentity:" prefix so callers can tell identity-capture
// failures apart from later refusal or config errors:
//   - [user.Current] failed (e.g. NSS lookup error in containers)
//   - [user.User.Gid] failed to parse as an int
//   - [user.User.GroupIds] failed or returned a malformed group id
//
// A GID parse failure is fatal rather than silently zeroed: GID 0 is the
// root group, and substituting it would invert the security posture this
// package enforces.
func CurrentIdentity() (Identity, error) {
	u, err := user.Current()
	if err != nil {
		return Identity{}, fmt.Errorf("system.CurrentIdentity: %w", err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return Identity{}, fmt.Errorf("system.CurrentIdentity: parsing gid %q: %w", u.Gid, err)
	}
	groupIDs, err := groupIDsIncludingPrimary(u, gid)
	if err != nil {
		return Identity{}, fmt.Errorf("system.CurrentIdentity: %w", err)
	}
	return Identity{
		EUID:     os.Geteuid(),
		GID:      gid,
		GroupIDs: groupIDs,
		Username: u.Username,
		Home:     u.HomeDir,
	}, nil
}

func groupIDsIncludingPrimary(u *user.User, primaryGID int) ([]int, error) {
	rawGroupIDs, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("loading group ids: %w", err)
	}

	seen := map[int]bool{primaryGID: true}
	groupIDs := []int{primaryGID}
	for _, rawGroupID := range rawGroupIDs {
		groupID, err := strconv.Atoi(rawGroupID)
		if err != nil {
			return nil, fmt.Errorf("parsing group id %q: %w", rawGroupID, err)
		}
		if seen[groupID] {
			continue
		}
		seen[groupID] = true
		groupIDs = append(groupIDs, groupID)
	}
	return groupIDs, nil
}
