// Package system implements the operating-system gates that PRD §11 and
// PRD §13 put in front of wdm startup and state-changing operations:
// refuse root/sudo execution, snapshot the invoking user, verify Docker
// group access, build a Docker/Compose readiness report from an injected
// version checker, and probe host CPU and total memory for install
// planning.
// Docker daemon reachability and Compose V2 parsing live in
// internal/docker. This package accepts a [DockerVersionCheck] function
// instead of importing that low-level package, so internal/core can wire
// the siblings together without turning system into a Docker wrapper.
// Import boundary: per, internal/system may be
// imported by other internal/* siblings and by cmd/wdm, but must not
// depend on pkg/engine, internal/tui, internal/cli, or internal/core. It
// imports only the standard library plus pkg/types for typed readiness
// errors; cmd/wdm translates [ErrRunningAsRoot] and [ErrRunningWithSudo]
// into a pkg/types.Error carrying ErrCodePermissionDenied so the process
// exits 6 (PRD §27).
// Public surface:
//   - [Identity] — effective UID, group memberships, username, and home directory of the invoker
//   - [CurrentIdentity] — capture an Identity from os.Geteuid + os/user.Current
//   - [ErrRunningAsRoot] — sentinel returned when the effective UID is 0
//   - [ErrRunningWithSudo] — sentinel returned when $SUDO_USER is set
//   - [RefuseRootOrSudo] — the PRD §11 gate; call before any state-changing op
//   - [DockerVersions] — normalized Docker/Compose version values from the injected checker
//   - [DockerVersionCheck] — readiness dependency supplied by the orchestrator
//   - [DockerReadiness] — Docker portion of the readiness report
//   - [HostResources] — CPU cores and total memory detected from the host
//   - [ReadinessReport] — combined Docker + host readiness snapshot
//   - [CheckReadiness] — Docker group, daemon/Compose, and host-resource gate
//   - [DetectHostResources] — host CPU + /proc/meminfo probe
package system
