package system

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrRunningAsRoot is returned by [RefuseRootOrSudo] when the effective
// UID is 0. Per PRD §11, wdm must never run as root: secret files, Docker
// socket access, and the managed stack tree all live under the invoking
// user's home, and a root-owned write would leave files the user cannot
// later edit without elevation.
// cmd/wdm wraps this sentinel via pkg/types.WrapError with
// pkg/types.ErrCodePermissionDenied so the process exits 6 (PRD §27
var ErrRunningAsRoot = errors.New("system: refusing to run as root")

// ErrRunningWithSudo is returned by [RefuseRootOrSudo] when $SUDO_USER is
// set to a non-empty value, marking a sudo launch (e.g. `sudo wdm` or
// `sudo -u alice wdm`). Even with a non-root effective UID — sudo can
// preserve a non-root identity via `--user` — $SUDO_USER signals that the
// launch crossed a privilege-elevation boundary, which PRD §11 forbids.
// cmd/wdm wraps this sentinel via pkg/types.WrapError with
// pkg/types.ErrCodePermissionDenied so the process exits 6 (PRD §27
var ErrRunningWithSudo = errors.New("system: refusing to run under sudo")

// RefuseRootOrSudo enforces PRD §11's twin refusal rules at process start.
// It returns nil for the supported invocation (a normal user account, no
// sudo in the launch chain), [ErrRunningAsRoot] when the effective UID is
// 0, and [ErrRunningWithSudo] when $SUDO_USER is set to a non-empty value.
// The checks are independent and order-stable: euid first, because process
// credentials cannot be forged from userland, then $SUDO_USER. It does no
// I/O and allocates only on the error path, so it is safe to call before
// any other initialization in main and must run before engine
// construction per Exit criteria.
func RefuseRootOrSudo() error {
	return refuseRootOrSudo(os.Geteuid(), os.Getenv("SUDO_USER"))
}

// refuseRootOrSudo is the pure core of [RefuseRootOrSudo], split out so
// unit tests in can exercise both branches without root
// privileges or environment mutation. Unexported because callers outside
// this package have no reason to bypass the live os.Geteuid / os.Getenv
// reads.
func refuseRootOrSudo(euid int, sudoUser string) error {
	if euid == 0 {
		return fmt.Errorf("%w: effective uid is 0", ErrRunningAsRoot)
	}
	if strings.TrimSpace(sudoUser) != "" {
		return fmt.Errorf("%w: SUDO_USER=%q", ErrRunningWithSudo, sudoUser)
	}
	return nil
}
