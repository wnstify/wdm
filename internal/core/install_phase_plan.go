package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/pkg/types"
)

func (e *Engine) planInstall(
	ctx context.Context,
	req types.InstallRequest,
	host system.HostResources,
	onProgress types.ProgressFn,
	tzDeps timezoneLookupDeps,
) (*installPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.AppID == "" {
		return nil, usageValidationError(
			"app id is required",
			"choose an app from the catalog",
			nil,
		)
	}
	if onProgress != nil {
		onProgress(types.StepInstallPlanning, 5, "planning install")
	}

	cat, err := e.loadInstallCatalog(ctx)
	if err != nil {
		return nil, err
	}
	app, err := selectCatalogApp(cat, req.AppID)
	if err != nil {
		return nil, err
	}

	stackPath, err := e.planInstallStackPath(req, app)
	if err != nil {
		return nil, err
	}
	probePort := e.probePort
	if probePort == nil {
		probePort = checkPortAvailable
	}
	plan := &installPlan{
		app:            app,
		stackPath:      stackPath,
		composeProject: "wdm-" + app.AppID,
		catalogChannel: e.settings.CatalogChannel,
		catalogVersion: cat.GeneratedAt.UTC().Format(time.RFC3339),
		resolvedValues: map[string]string{},
		localPorts:     []types.PortBinding{},
		probePort:      probePort,
		portOverrides:  req.PortOverrides,
	}

	if err := plan.planPlaceholders(req, e.settings.Timezone, tzDeps); err != nil {
		return nil, err
	}
	if err := plan.planPorts(ctx); err != nil {
		return nil, err
	}
	if err := plan.planResources(req, host, onProgress); err != nil {
		return nil, err
	}
	return plan, nil
}

func (e *Engine) loadInstallCatalog(ctx context.Context) (*catalog.Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	channel := e.settings.CatalogChannel
	if channel == "" || !fs.ValidPath(channel) || strings.Contains(channel, "/") || channel == "." {
		return nil, usageValidationError(
			"catalog channel is invalid",
			"set catalog_channel to stable in config.toml",
			fmt.Errorf("invalid catalog channel %q", channel),
		)
	}

	catalogPath := path.Join(channel, "catalog.yaml")
	raw, err := fs.ReadFile(e.installCatalogFS(), catalogPath)
	if err != nil {
		return nil, catalogVerificationError(
			"catalog could not be read",
			"install the stable catalog before running apps install",
			err,
		)
	}
	cat, err := catalog.LoadCatalogBytes(ctx, raw)
	if err != nil {
		return nil, catalogVerificationError(
			"catalog could not be verified",
			"refresh the catalog and retry",
			err,
		)
	}
	return cat, nil
}

func (e *Engine) installCatalogFS() fs.FS {
	if e.catalog != nil {
		return e.catalog
	}
	return os.DirFS(filepath.Join(e.dataDir, "catalogs"))
}

func selectCatalogApp(cat *catalog.Catalog, appID string) (catalog.App, error) {
	var selected catalog.App
	found := false
	for _, app := range cat.Apps {
		if app.AppID != appID {
			continue
		}
		if found {
			return catalog.App{}, catalogVerificationError(
				"catalog contains duplicate app ids",
				"refresh the catalog and retry",
				fmt.Errorf("duplicate app_id %q", appID),
			)
		}
		selected = app
		found = true
	}
	if !found {
		return catalog.App{}, usageValidationError(
			"app is not available in the selected catalog",
			"run apps list and choose one of the listed app ids",
			fmt.Errorf("unknown app_id %q", appID),
		)
	}
	return selected, nil
}

func (e *Engine) planInstallStackPath(req types.InstallRequest, app catalog.App) (string, error) {
	if err := security.RejectUnsafeRoot(e.stackBase); err != nil {
		return "", usageValidationError(
			"stack base path is unsafe",
			"choose a stack base under your home directory",
			err,
		)
	}

	if req.StackPath == "" {
		stackPath, err := security.SafeJoin(e.stackBase, app.AppID)
		if err != nil {
			return "", usageValidationError(
				"stack path is unsafe",
				"choose a stack path under the configured stack base",
				err,
			)
		}
		return stackPath, nil
	}

	if hasTraversalSegment(req.StackPath) {
		return "", usageValidationError(
			"stack path must not contain parent traversal",
			"remove any .. path segments from --stack-path",
			fmt.Errorf("stack path %q contains parent traversal", req.StackPath),
		)
	}
	expanded, err := expandHome(req.StackPath)
	if err != nil {
		return "", fmt.Errorf("core.install: expanding stack path: %w", err)
	}
	if !filepath.IsAbs(expanded) {
		return "", usageValidationError(
			"stack path must be absolute",
			"pass an absolute --stack-path",
			fmt.Errorf("stack path %q is not absolute", req.StackPath),
		)
	}
	if err := security.RejectUnsafeRoot(expanded); err != nil {
		return "", usageValidationError(
			"stack path is unsafe",
			"choose a stack path under your home directory",
			err,
		)
	}
	return filepath.Clean(expanded), nil
}

func hasTraversalSegment(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func (p *installPlan) planPlaceholders(
	req types.InstallRequest,
	settingsTimezone string,
	tzDeps timezoneLookupDeps,
) error {
	declared := make(map[string]catalog.Placeholder, len(p.app.Placeholders))
	for _, ph := range p.app.Placeholders {
		if _, ok := declared[ph.Name]; ok {
			return catalogVerificationError(
				"catalog contains duplicate placeholders",
				"refresh the catalog and retry",
				fmt.Errorf("duplicate placeholder %q", ph.Name),
			)
		}
		declared[ph.Name] = ph
		p.placeholders = append(p.placeholders, render.Placeholder{
			Name:        ph.Name,
			Type:        render.Type(ph.Type),
			Required:    ph.Required,
			Default:     ph.Default,
			Regenerable: ph.Regenerable,
		})
	}

	for key := range req.PlaceholderValues {
		if _, ok := declared[key]; !ok {
			return usageValidationError(
				"placeholder value is not declared by the catalog",
				"remove unknown placeholder values from the install request",
				fmt.Errorf("placeholder %q is not declared", key),
			)
		}
	}

	for _, ph := range p.app.Placeholders {
		value, hasRequestValue := req.PlaceholderValues[ph.Name]
		var err error
		switch render.Type(ph.Type) {
		case render.TypeSecret:
			p.generatedFields = append(p.generatedFields, ph.Name)
			continue
		case render.TypeDomain:
			if !hasRequestValue {
				value = req.Domain
			}
			value, err = resolveDomainPlaceholder(ph, value)
		case render.TypeTimezone:
			if !hasRequestValue {
				value = settingsTimezone
			}
			value, err = resolveTimezone(value, p.stackPath, tzDeps)
		case render.TypePath:
			value, err = p.resolvePathPlaceholder(ph, value, hasRequestValue)
		case render.TypeString:
			value, err = resolveStringPlaceholder(ph, value, hasRequestValue)
		case render.TypeBool:
			value, err = resolveBoolPlaceholder(ph, value, hasRequestValue)
		case render.TypePort:
			value, err = resolvePortPlaceholder(ph, value, hasRequestValue)
		default:
			err = catalogVerificationError(
				"catalog contains an unknown placeholder type",
				"refresh the catalog and retry",
				fmt.Errorf("placeholder %q has type %q", ph.Name, ph.Type),
			)
		}
		if err != nil {
			return err
		}
		if render.Type(ph.Type) == render.TypeDomain && p.selectedDomain == "" {
			p.selectedDomain = value
		}
		p.resolvedValues[ph.Name] = value
	}

	if err := p.addSyntheticResolvedValue("UID", strconv.Itoa(os.Getuid())); err != nil {
		return err
	}
	if err := p.addSyntheticResolvedValue("GID", strconv.Itoa(os.Getgid())); err != nil {
		return err
	}
	return p.addSyntheticResolvedValue(dockerSocketSourceValueName, resolveDockerSocketSource())
}

// dockerSocketSourceValueName is the built-in template var carrying the host
// path of the rootless Docker socket, bound as the source of a socket-proxy
// sidecar's docker.sock mount (issue #134). Reserved like UID/GID.
const dockerSocketSourceValueName = "DOCKER_SOCKET_SOURCE"

// resolveDockerSocketSource returns the host path of the rootless Docker
// daemon socket. wdm operates only against a rootless daemon (PRD §11, issue
// #135), so the source is always the per-user socket: $XDG_RUNTIME_DIR/
// docker.sock when the runtime dir is set, otherwise /run/user/<uid>/
// docker.sock. The socket-proxy template binds this as the read-only source so
// the proxy works under rootless Docker, where /var/run/docker.sock is absent
// or the inaccessible rootful socket (issue #134).
func resolveDockerSocketSource() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); dir != "" {
		return filepath.Join(dir, "docker.sock")
	}
	return fmt.Sprintf("/run/user/%d/docker.sock", os.Getuid())
}

func (p *installPlan) addSyntheticResolvedValue(name, value string) error {
	if _, ok := p.resolvedValues[name]; ok {
		return catalogVerificationError(
			"catalog placeholder collides with a built-in template value",
			"refresh the catalog and retry",
			fmt.Errorf("placeholder %q is reserved by wdm", name),
		)
	}
	p.placeholders = append(p.placeholders, render.Placeholder{
		Name:     name,
		Type:     render.TypeString,
		Required: true,
	})
	p.resolvedValues[name] = value
	return nil
}

func resolveDomainPlaceholder(ph catalog.Placeholder, value string) (string, error) {
	if value == "" && ph.Required {
		return "", usageValidationError(
			"domain is required",
			"pass a domain for the selected app",
			fmt.Errorf("placeholder %q is required", ph.Name),
		)
	}
	normalized, err := normalizeDomain(value)
	if err != nil {
		return "", usageValidationError(
			"domain is invalid",
			"pass an ASCII hostname such as app.example.com",
			err,
		)
	}
	return normalized, nil
}

func normalizeDomain(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("domain must not be empty")
	}
	if strings.Contains(value, "://") || strings.ContainsAny(value, "/:@") {
		return "", fmt.Errorf("domain %q must be a hostname, not a URL", value)
	}
	if err := validateDomainASCII(value); err != nil {
		return "", err
	}
	host := strings.ToLower(strings.TrimSuffix(value, "."))
	if host == "" || host == "localhost" || strings.HasPrefix(host, "*.") {
		return "", fmt.Errorf("domain %q is not allowed", value)
	}
	if ip := net.ParseIP(host); ip != nil {
		return "", fmt.Errorf("domain %q must not be an IP literal", value)
	}
	if len(host) > 253 {
		return "", fmt.Errorf("domain %q is too long", value)
	}
	if err := validateDomainLabels(host); err != nil {
		return "", err
	}
	return host, nil
}

func validateDomainASCII(value string) error {
	for _, r := range value {
		if r > 127 {
			return fmt.Errorf("domain %q must be ASCII", value)
		}
	}
	return nil
}

func validateDomainLabels(host string) error {
	for _, label := range strings.Split(host, ".") {
		if err := validateDomainLabel(label); err != nil {
			return err
		}
	}
	return nil
}

func validateDomainLabel(label string) error {
	if len(label) == 0 || len(label) > 63 {
		return fmt.Errorf("domain label %q has invalid length", label)
	}
	for i, r := range label {
		isLetter := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		isHyphen := r == '-'
		if !isLetter && !isDigit && !isHyphen {
			return fmt.Errorf("domain label %q contains invalid character %q", label, r)
		}
		if isHyphen && (i == 0 || i == len(label)-1) {
			return fmt.Errorf("domain label %q must not start or end with hyphen", label)
		}
	}
	return nil
}

func resolveTimezone(value, _ string, deps timezoneLookupDeps) (string, error) {
	deps = completeTimezoneLookupDeps(deps)
	if value != "" {
		return validateTimezone(value, deps)
	}
	if envTZ, ok := deps.LookupEnv("TZ"); ok && strings.TrimSpace(envTZ) != "" {
		return validateTimezone(strings.TrimSpace(envTZ), deps)
	}
	if raw, err := deps.ReadFile("/etc/timezone"); err == nil {
		if tz := strings.TrimSpace(string(raw)); tz != "" {
			return validateTimezone(tz, deps)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", usageValidationError(
			"timezone could not be detected",
			"set timezone in config.toml",
			err,
		)
	}
	if link, err := deps.ReadLink("/etc/localtime"); err == nil {
		if tz, ok := timezoneFromLocaltimeLink(link); ok {
			return validateTimezone(tz, deps)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", usageValidationError(
			"timezone could not be detected",
			"set timezone in config.toml",
			err,
		)
	}
	return "", types.NewError(
		types.ErrCodeUsageValidation,
		"timezone could not be detected",
		"set timezone in config.toml",
	)
}

func completeTimezoneLookupDeps(deps timezoneLookupDeps) timezoneLookupDeps {
	if deps.LookupEnv == nil {
		deps.LookupEnv = os.LookupEnv
	}
	if deps.ReadFile == nil {
		deps.ReadFile = os.ReadFile
	}
	if deps.ReadLink == nil {
		deps.ReadLink = os.Readlink
	}
	if deps.LoadLocation == nil {
		deps.LoadLocation = time.LoadLocation
	}
	return deps
}

func validateTimezone(value string, deps timezoneLookupDeps) (string, error) {
	tz := strings.TrimSpace(value)
	if tz == "" {
		return "", types.NewError(
			types.ErrCodeUsageValidation,
			"timezone is invalid",
			"set timezone in config.toml",
		)
	}
	if _, err := deps.LoadLocation(tz); err != nil {
		return "", usageValidationError(
			"timezone is invalid",
			"set timezone to a valid IANA timezone such as Europe/Bratislava",
			err,
		)
	}
	return tz, nil
}

func timezoneFromLocaltimeLink(link string) (string, bool) {
	const marker = "zoneinfo/"
	idx := strings.LastIndex(link, marker)
	if idx < 0 {
		return "", false
	}
	tz := strings.TrimPrefix(link[idx:], marker)
	return tz, tz != ""
}

func (p *installPlan) resolvePathPlaceholder(ph catalog.Placeholder, value string, hasRequestValue bool) (string, error) {
	if !hasRequestValue || value == "" {
		if ph.Required {
			return "", usageValidationError(
				"path placeholder is required",
				"pass the required host path for this app",
				fmt.Errorf("placeholder %q is required", ph.Name),
			)
		}
		defaultValue, ok := stringDefault(ph.Default)
		if !ok || defaultValue == "" {
			return "", nil
		}
		value = defaultValue
	}
	expanded, err := expandHome(value)
	if err != nil {
		return "", fmt.Errorf("core.install: expanding path placeholder %q: %w", ph.Name, err)
	}
	if !filepath.IsAbs(expanded) {
		return "", usageValidationError(
			"path placeholder must be absolute",
			"pass an absolute host path",
			fmt.Errorf("placeholder %q has relative path %q", ph.Name, value),
		)
	}
	resolved, err := filepath.EvalSymlinks(expanded)
	if err != nil {
		return "", usageValidationError(
			"path placeholder does not exist",
			"create the host path or choose an existing directory",
			err,
		)
	}
	if err := security.EnsureWithinRoot(filepath.Clean(p.stackPath), filepath.Clean(resolved)); err == nil {
		return "", usageValidationError(
			"path placeholder must be outside the stack directory",
			"choose a host path outside the managed stack",
			fmt.Errorf("placeholder %q path %q is inside stack %q", ph.Name, resolved, p.stackPath),
		)
	}
	return resolved, nil
}

func resolveStringPlaceholder(ph catalog.Placeholder, value string, hasRequestValue bool) (string, error) {
	if hasRequestValue {
		return value, validateStringPlaceholderValue(ph.Name, value)
	}
	if value, ok := stringDefault(ph.Default); ok {
		return value, validateStringPlaceholderValue(ph.Name, value)
	}
	if ph.Required {
		return "", usageValidationError(
			"placeholder value is required",
			"pass all required placeholder values for this app",
			fmt.Errorf("placeholder %q is required", ph.Name),
		)
	}
	return "", nil
}

// validateStringPlaceholderValue rejects CR/LF/NUL in a string placeholder
// value before it reaches the .env template. These control characters have no
// legitimate place in an env value and a newline would let a single --set value
// inject extra KEY=VALUE lines (overriding later secrets), so it fails closed.
func validateStringPlaceholderValue(name, value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return usageValidationError(
			"placeholder value contains control characters",
			"remove carriage return, newline, or NUL characters from the value",
			fmt.Errorf("placeholder %q value contains a control character", name),
		)
	}
	return nil
}

func resolveBoolPlaceholder(ph catalog.Placeholder, value string, hasRequestValue bool) (string, error) {
	if !hasRequestValue {
		if ph.Default != nil {
			value = fmt.Sprint(ph.Default)
		} else if ph.Required {
			return "", usageValidationError(
				"boolean placeholder value is required",
				"pass true or false",
				fmt.Errorf("placeholder %q is required", ph.Name),
			)
		} else {
			// Optional bool, no default, no request value: short-circuit to ""
			// like resolveStringPlaceholder, rather than falling through to
			// ParseBool("") which would always error.
			return "", nil
		}
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return "", usageValidationError(
			"boolean placeholder value is invalid",
			"pass true or false",
			err,
		)
	}
	return strconv.FormatBool(parsed), nil
}

func resolvePortPlaceholder(ph catalog.Placeholder, value string, hasRequestValue bool) (string, error) {
	if !hasRequestValue {
		if ph.Default != nil {
			value = fmt.Sprint(ph.Default)
		} else if ph.Required {
			return "", usageValidationError(
				"port placeholder value is required",
				"pass a port between 1 and 65535",
				fmt.Errorf("placeholder %q is required", ph.Name),
			)
		} else {
			// Optional port, no default, no request value: short-circuit to ""
			// like resolveStringPlaceholder, rather than falling through to
			// Atoi("") which would always error.
			return "", nil
		}
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return "", usageValidationError(
			"port placeholder value is invalid",
			"pass a port between 1 and 65535",
			fmt.Errorf("placeholder %q has invalid port %q", ph.Name, value),
		)
	}
	return strconv.Itoa(port), nil
}

func stringDefault(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	return fmt.Sprint(v), true
}

// planPorts builds the local port bindings for the install plan from the
// verified catalog. The bind interface is derived SOLELY from the catalog
// port.public field (PRD §11.1): a public port binds 0.0.0.0, every other
// port binds 127.0.0.1. No InstallRequest field influences the interface,
// so a user cannot force a public bind — only a signed catalog can. Range
// ports (host_range/container_range) are expanded to one binding per port
// so availability probing, the deploy confirmation, and the rendered-compose
// public-bind scan all operate on the exact set of ports Compose will bind.
// A public declaration for an app admin/web-UI port is refused as a
// defense-in-depth backstop (PRD §11.1(d)) before any port is probed.
func (p *installPlan) planPorts(ctx context.Context) error {
	seen := map[string]struct{}{}
	var planned []types.PortBinding
	publicPorts := map[int]struct{}{}
	rangeHostPorts := map[int]struct{}{}
	for _, port := range p.app.Ports {
		bindings, err := portBindings(port)
		if err != nil {
			return err
		}
		isRange := port.HostRange != "" || port.ContainerRange != ""
		for _, binding := range bindings {
			key := fmt.Sprintf("%s/%d", binding.Protocol, binding.HostPort)
			if _, ok := seen[key]; ok {
				return catalogVerificationError(
					"catalog contains duplicate host ports",
					"refresh the catalog and retry",
					fmt.Errorf("duplicate host port %s", key),
				)
			}
			seen[key] = struct{}{}
			if port.Public {
				publicPorts[binding.HostPort] = struct{}{}
			}
			if isRange {
				rangeHostPorts[binding.HostPort] = struct{}{}
			}
			planned = append(planned, binding)
		}
	}

	// Admin-port detection falls back to the first planned port when the app
	// declares no local_target_url_template, so identify admin ports only
	// after the full plan is known. The refusal precedes any availability
	// probe so a catalog defect fails fast (PRD §11.1(d)).
	if err := refusePublicAdminPorts(p, planned, publicPorts); err != nil {
		return err
	}

	// Apply user remaps before the probe so the chosen port is the one
	// probed (ADR 0004). Only single loopback ports are remappable.
	if err := applyPortOverrides(planned, p.portOverrides, rangeHostPorts, publicPorts); err != nil {
		return err
	}

	plannedHostPorts := make(map[int]struct{}, len(planned))
	for _, binding := range planned {
		plannedHostPorts[binding.HostPort] = struct{}{}
	}

	for _, binding := range planned {
		if err := p.probePort(ctx, binding); err != nil {
			return p.enrichPortConflict(ctx, binding, rangeHostPorts, publicPorts, plannedHostPorts, err)
		}
		p.localPorts = append(p.localPorts, binding)
	}
	return nil
}

// applyPortOverrides rewrites planned host ports per the request's
// oldHostPort→newHostPort map, before the availability probe (ADR 0004). Only
// single loopback ports are remappable: an override naming a range port, a
// public port, or no planned binding is a usage-validation error, as is a
// privileged (≤1024) or out-of-range target (PRD §11). The host IP is never
// changed — a remap can never turn a loopback port into a public one.
func applyPortOverrides(planned []types.PortBinding, overrides map[int]int, rangeHostPorts, publicPorts map[int]struct{}) error {
	if len(overrides) == 0 {
		return nil
	}

	// Resolve and validate every override against the pre-mutation plan first,
	// so a remap whose target equals another remap's source cannot reorder by
	// map-iteration order. Only after all overrides validate are the rewrites
	// applied.
	type rewrite struct {
		idx     int
		newPort int
	}
	rewrites := make([]rewrite, 0, len(overrides))
	for oldPort, newPort := range overrides {
		idx := -1
		for i := range planned {
			if planned[i].HostPort == oldPort {
				idx = i
				break
			}
		}
		if idx == -1 {
			return usageValidationError(
				"port override names no planned host port",
				fmt.Sprintf("no app port binds 127.0.0.1:%d; pass --port with a port this app actually binds", oldPort),
				fmt.Errorf("override %d→%d matches no planned binding", oldPort, newPort),
			)
		}
		if _, isRange := rangeHostPorts[oldPort]; isRange {
			return usageValidationError(
				"port override targets a range port",
				fmt.Sprintf("host port %d belongs to a port range and cannot be remapped", oldPort),
				fmt.Errorf("override %d→%d targets a range host port", oldPort, newPort),
			)
		}
		if _, isPublic := publicPorts[oldPort]; isPublic {
			return usageValidationError(
				"port override targets a public port",
				fmt.Sprintf("host port %d is a public port and cannot be remapped", oldPort),
				fmt.Errorf("override %d→%d targets a public host port", oldPort, newPort),
			)
		}
		if newPort <= 1024 || newPort > 65535 {
			return usageValidationError(
				"port override target is out of range",
				fmt.Sprintf("choose an unprivileged host port between 1025 and 65535, not %d", newPort),
				fmt.Errorf("override %d→%d target is not in 1025..65535", oldPort, newPort),
			)
		}
		rewrites = append(rewrites, rewrite{idx: idx, newPort: newPort})
	}
	for _, rw := range rewrites {
		planned[rw.idx].HostPort = rw.newPort
	}

	// A remap must not land two bindings on the same protocol/host port; that
	// would silently install on an unintended port instead of the requested
	// one. Reject it as a usage error.
	seen := make(map[string]struct{}, len(planned))
	for _, binding := range planned {
		key := fmt.Sprintf("%s/%d", binding.Protocol, binding.HostPort)
		if _, dup := seen[key]; dup {
			return usageValidationError(
				"port override collides with another planned port",
				fmt.Sprintf("two services would bind %s; choose distinct host ports", key),
				fmt.Errorf("override produced duplicate host port %s", key),
			)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// enrichPortConflict turns a plan-time probe failure into a typed
// [types.PortConflictError] carrying a deterministic suggestion when the
// conflicting binding is a remappable single loopback port. An EACCES
// (elevated-privileges) failure, and any conflict on a range or public port,
// stay the plain fail-closed error unchanged (ADR 0004). The pre-deploy
// re-check ([recheckPorts]) never calls this, so its rare race stays plain too.
func (p *installPlan) enrichPortConflict(
	ctx context.Context,
	binding types.PortBinding,
	rangeHostPorts, publicPorts, plannedHostPorts map[int]struct{},
	probeErr error,
) error {
	if errors.Is(probeErr, syscall.EACCES) {
		return probeErr
	}
	if binding.HostIP != "127.0.0.1" {
		return probeErr
	}
	if _, isRange := rangeHostPorts[binding.HostPort]; isRange {
		return probeErr
	}
	if _, isPublic := publicPorts[binding.HostPort]; isPublic {
		return probeErr
	}

	suggested := p.suggestFreePort(ctx, binding, plannedHostPorts)
	hint := fmt.Sprintf("free 127.0.0.1:%d or remap it with --port %d=NEW", binding.HostPort, binding.HostPort)
	if suggested != 0 {
		hint = fmt.Sprintf("127.0.0.1:%d is in use; remap it with --port %d=%d (or another free port)", binding.HostPort, binding.HostPort, suggested)
	}

	// probeErr is already a usage_validation *Error; wrap its underlying cause
	// (the net.OpError → syscall chain) so the enriched error keeps errors.Is
	// reachability without duplicating the "[usage_validation]" message frame.
	cause := probeErr
	var probeTyped *types.Error
	if errors.As(probeErr, &probeTyped) && probeTyped.Cause != nil {
		cause = probeTyped.Cause
	}
	return types.NewPortConflictError(
		binding.Service,
		binding.ContainerPort,
		binding.HostPort,
		suggested,
		types.WrapError(types.ErrCodeUsageValidation, "local port is already in use", hint, cause),
	)
}

// suggestFreePort scans upward from the conflicting port for the next free,
// unprivileged (>1024) loopback host port, skipping ports already planned by
// the same install, re-probing each candidate through the same seam as the
// plan-time check (no new TOCTOU hole). It returns 0 fail-closed when no free
// port is found in the scan range (ADR 0004 / PRD §11).
func (p *installPlan) suggestFreePort(ctx context.Context, conflict types.PortBinding, plannedHostPorts map[int]struct{}) int {
	start := conflict.HostPort + 1
	if start <= 1024 {
		start = 1025
	}
	for candidate := start; candidate <= 65535; candidate++ {
		if _, planned := plannedHostPorts[candidate]; planned {
			continue
		}
		probe := conflict
		probe.HostPort = candidate
		if p.probePort(ctx, probe) == nil {
			return candidate
		}
	}
	return 0
}

// refusePublicAdminPorts refuses any public-declared host port that is also
// the app's web-UI/admin surface (PRD §11.1(d)). Admin surfaces stay
// localhost-only and front a reverse proxy; the primary protection is that
// admin ports are simply not declared public, and this is the backstop.
func refusePublicAdminPorts(plan *installPlan, planned []types.PortBinding, publicPorts map[int]struct{}) error {
	if len(publicPorts) == 0 {
		return nil
	}
	adminPorts, err := identifyAdminHostPorts(plan, planned, publicPorts)
	if err != nil {
		return err
	}
	for _, binding := range planned {
		if _, isPublic := publicPorts[binding.HostPort]; !isPublic {
			continue
		}
		if _, isAdmin := adminPorts[binding.HostPort]; !isAdmin {
			continue
		}
		return catalogVerificationError(
			"catalog declares an admin port public",
			"keep the web-UI/admin port localhost-only and front it with a reverse proxy",
			fmt.Errorf(
				"service %q host port %d is the app's web-UI/admin surface and must not be declared public",
				binding.Service,
				binding.HostPort,
			),
		)
	}
	return nil
}

// portBindings expands one catalog port entry into its concrete host
// bindings. A plain entry (no range fields) yields one binding from
// Host/Container. A range entry carries host_range/container_range alongside
// Host/Container, where Host and Container are the range low ends (the schema
// contract): it is expanded to one binding per port in the span. A range whose
// Host/Container do not equal the declared range low ends is a malformed
// mix and is refused. The bind interface is set from port.public per PRD §11.1.
func portBindings(port catalog.Port) ([]types.PortBinding, error) {
	protocol := port.Protocol
	if protocol == "" {
		protocol = "tcp"
	}
	hostIP := "127.0.0.1"
	if port.Public {
		hostIP = "0.0.0.0"
	}

	if port.HostRange != "" || port.ContainerRange != "" {
		return rangePortBindings(port, protocol, hostIP)
	}

	if port.Host < 1 || port.Host > 65535 || port.Container < 1 || port.Container > 65535 {
		return nil, catalogVerificationError(
			"catalog contains an invalid port",
			"refresh the catalog and retry",
			fmt.Errorf("service %q has host/container ports %d/%d", port.Service, port.Host, port.Container),
		)
	}
	return []types.PortBinding{{
		Service:       port.Service,
		HostIP:        hostIP,
		HostPort:      port.Host,
		ContainerPort: port.Container,
		Protocol:      protocol,
	}}, nil
}

// rangePortBindings validates a host_range/container_range pair and expands
// it to one binding per port. Both bounds must lie in 1..65535, lo<=hi, and
// the host and container spans must be equal length so the port-for-port
// mapping is well defined (the contract documented on [catalog.Port]). The
// schema pairs each range with Host/Container; those must equal the range low
// ends, otherwise the entry is a malformed single+range mix.
func rangePortBindings(port catalog.Port, protocol, hostIP string) ([]types.PortBinding, error) {
	if port.HostRange == "" || port.ContainerRange == "" {
		return nil, catalogVerificationError(
			"catalog port range is incomplete",
			"refresh the catalog and retry",
			fmt.Errorf(
				"service %q must declare both host_range and container_range (got %q/%q)",
				port.Service,
				port.HostRange,
				port.ContainerRange,
			),
		)
	}
	hostLo, hostHi, err := parsePortRange(port.Service, port.HostRange)
	if err != nil {
		return nil, err
	}
	containerLo, containerHi, err := parsePortRange(port.Service, port.ContainerRange)
	if err != nil {
		return nil, err
	}
	if hostHi-hostLo != containerHi-containerLo {
		return nil, catalogVerificationError(
			"catalog port range spans do not match",
			"refresh the catalog and retry",
			fmt.Errorf(
				"service %q host range %q and container range %q have different lengths",
				port.Service,
				port.HostRange,
				port.ContainerRange,
			),
		)
	}
	// The schema sets Host/Container to the range low ends; a contradiction is
	// a malformed single+range mix that would make the bound ambiguous.
	if (port.Host != 0 && port.Host != hostLo) || (port.Container != 0 && port.Container != containerLo) {
		return nil, catalogVerificationError(
			"catalog port mixes single and range declarations",
			"refresh the catalog and retry",
			fmt.Errorf(
				"service %q single ports (%d/%d) do not match range low ends (%d/%d)",
				port.Service,
				port.Host,
				port.Container,
				hostLo,
				containerLo,
			),
		)
	}
	bindings := make([]types.PortBinding, 0, hostHi-hostLo+1)
	for offset := 0; hostLo+offset <= hostHi; offset++ {
		bindings = append(bindings, types.PortBinding{
			Service:       port.Service,
			HostIP:        hostIP,
			HostPort:      hostLo + offset,
			ContainerPort: containerLo + offset,
			Protocol:      protocol,
		})
	}
	return bindings, nil
}

// parsePortRange parses an inclusive "<lo>-<hi>" port span, enforcing both
// bounds in 1..65535 and lo<=hi. A malformed span is a catalog defect.
func parsePortRange(service, spec string) (lo, hi int, err error) {
	rangeErr := func(cause error) error {
		return catalogVerificationError(
			"catalog contains an invalid port range",
			"refresh the catalog and retry",
			cause,
		)
	}
	before, after, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, rangeErr(fmt.Errorf("service %q has malformed port range %q", service, spec))
	}
	lo, loErr := strconv.Atoi(before)
	hi, hiErr := strconv.Atoi(after)
	if loErr != nil || hiErr != nil {
		return 0, 0, rangeErr(fmt.Errorf("service %q has non-numeric port range %q", service, spec))
	}
	if lo < 1 || lo > 65535 || hi < 1 || hi > 65535 {
		return 0, 0, rangeErr(fmt.Errorf("service %q port range %q is out of the 1..65535 bounds", service, spec))
	}
	if lo > hi {
		return 0, 0, rangeErr(fmt.Errorf("service %q port range %q has lo greater than hi", service, spec))
	}
	return lo, hi, nil
}

// identifyAdminHostPorts collects the host ports wdm treats as the app's
// web-UI/admin surface (PRD §11.1(d)). The primary signal is the host port
// embedded in the rendered local_target_url_template; when the app declares
// no template, the local target URL falls back to the first NON-public planned
// port, so that port is the admin surface. A public-declared port is, by
// §11.1, deliberately public and is never the admin surface, so the fallback
// skips public ports — otherwise a public-first app whose first port is its
// data port (no web UI, no local_target_url_template) would be mis-refused. If
// every planned port is public, the fallback contributes no admin port. The
// PangolinGuidance.TargetURL host port is included when set and parseable.
// These ports must stay localhost-only, so a public declaration for any of
// them is refused.
func identifyAdminHostPorts(
	plan *installPlan,
	planned []types.PortBinding,
	publicPorts map[int]struct{},
) (map[int]struct{}, error) {
	admin := map[int]struct{}{}
	if plan.app.LocalTargetURLTemplate == "" {
		for _, binding := range planned {
			if _, isPublic := publicPorts[binding.HostPort]; isPublic {
				continue
			}
			admin[binding.HostPort] = struct{}{}
			break
		}
	} else {
		localTargetURL, err := renderInstallLocalTargetURL(plan)
		if err != nil {
			return nil, err
		}
		if port, ok := hostPortFromURL(localTargetURL); ok {
			admin[port] = struct{}{}
		}
	}
	if port, ok := hostPortFromURL(plan.app.PangolinGuidance.TargetURL); ok {
		admin[port] = struct{}{}
	}
	return admin, nil
}

// hostPortFromURL extracts the numeric host port from a service URL, if
// present and parseable. A URL without an explicit port (and any value that
// does not parse) yields ok=false, because no admin port can be derived from
// it — the public-bind refusal then has nothing to match against.
func hostPortFromURL(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return 0, false
	}
	portText := parsed.Port()
	if portText == "" {
		return 0, false
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, false
	}
	return port, true
}

// recheckPorts re-verifies every planned localhost port immediately
// before the deployment point — the second of the two
// checks that closes the TOCTOU window between planning and
// `docker compose up -d`. Conflicts surface as
// [types.ErrCodeUsageValidation] with the port named in the hint.
func (p *installPlan) recheckPorts(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, binding := range p.localPorts {
		if err := p.probePort(ctx, binding); err != nil {
			return err
		}
	}
	return nil
}

func checkPortAvailable(ctx context.Context, binding types.PortBinding) error {
	addr := net.JoinHostPort(binding.HostIP, strconv.Itoa(binding.HostPort))
	var listenConfig net.ListenConfig
	switch binding.Protocol {
	case "tcp":
		ln, err := listenConfig.Listen(ctx, "tcp", addr)
		if err != nil {
			return classifyPortBindError(binding.HostPort, err)
		}
		if err := ln.Close(); err != nil {
			return fmt.Errorf("core.install: closing port probe listener: %w", err)
		}
	case "udp":
		conn, err := listenConfig.ListenPacket(ctx, "udp", addr)
		if err != nil {
			return classifyPortBindError(binding.HostPort, err)
		}
		if err := conn.Close(); err != nil {
			return fmt.Errorf("core.install: closing port probe listener: %w", err)
		}
	default:
		return catalogVerificationError(
			"catalog contains an invalid port protocol",
			"refresh the catalog and retry",
			fmt.Errorf("service %q has protocol %q", binding.Service, binding.Protocol),
		)
	}
	return nil
}

// classifyPortBindError turns a failed localhost-port probe into a
// typed [types.ErrCodeUsageValidation] error, distinguishing an EACCES
// bind (the port needs elevated privileges, e.g. a curated sub-1024
// host port) from an already-in-use bind. wdm runs unprivileged by
// invariant (PRD §11), so a sub-1024 bind reports honestly that the
// port requires elevated privileges and the hint suggests an
// unprivileged (>1024) port rather than the misleading "already in
// use" text. Every other bind failure keeps the byte-compatible
// already-in-use message. The error code is the same on both arms so
// the PRD §27 exit-code mapping is unchanged.
// It is split out from [checkPortAvailable] so the classification can
// be unit-tested against constructed wrapped syscall errors — a real
// sub-1024 bind is not portable (macOS permits unprivileged low-port
// binds and CI may run as root). errors.Is walks the net.OpError →
// os.SyscallError → syscall.Errno chain that the listener returns.
func classifyPortBindError(hostPort int, err error) error {
	if errors.Is(err, syscall.EACCES) {
		return usageValidationError(
			"local port requires elevated privileges",
			fmt.Sprintf(
				"127.0.0.1:%d needs elevated privileges to bind; choose an unprivileged port above 1024",
				hostPort,
			),
			err,
		)
	}
	return usageValidationError(
		"local port is already in use",
		fmt.Sprintf("free 127.0.0.1:%d or choose another port", hostPort),
		err,
	)
}

func (p *installPlan) planResources(
	req types.InstallRequest,
	host system.HostResources,
	onProgress types.ProgressFn,
) error {
	if len(p.app.Resources) == 0 {
		return nil
	}
	if err := validateInstallResourceInputs(req, host); err != nil {
		return err
	}
	profiles, err := indexResourceProfiles(p.app.Resources)
	if err != nil {
		return err
	}

	recMemory, recCPU, err := sumResourceBand(p.app.Resources, types.ResourceProfileRecommended)
	if err != nil {
		return err
	}
	// Recommended totals are persisted into .wdm.lock so status, update, and
	// future planning surfaces can report the catalog's normal sizing guidance.
	// They are not a hard host-capacity reservation; Docker resource limits are
	// caps, not guaranteed allocations.
	p.recommendedResources = &state.RecommendedResources{
		MemoryBytes: recMemory,
		CPUs:        recCPU,
	}

	useMin, err := p.chooseMinimumResourceProfile(req, host, onProgress)
	if err != nil {
		return err
	}
	selected := selectResourceProfiles(p.app.Resources, useMin)
	if err := applyResourceOverrides(selected, profiles, req.ResourceOverrides); err != nil {
		return err
	}
	return p.addSelectedResourceValues(selected)
}

func validateInstallResourceInputs(req types.InstallRequest, host system.HostResources) error {
	if host.CPUCores <= 0 || host.TotalMemoryBytes == 0 {
		return usageValidationError(
			"host resources could not be detected",
			"run wdm on a supported host or retry after fixing host resource detection",
			fmt.Errorf("cpu=%d memory=%d", host.CPUCores, host.TotalMemoryBytes),
		)
	}
	if req.ResourceProfile == "" ||
		req.ResourceProfile == types.ResourceProfileRecommended ||
		req.ResourceProfile == types.ResourceProfileMin {
		return nil
	}
	return usageValidationError(
		"resource profile is invalid",
		"choose recommended or min",
		fmt.Errorf("unknown resource profile %q", req.ResourceProfile),
	)
}

func indexResourceProfiles(resources []catalog.ResourceProfile) (map[string]catalog.ResourceProfile, error) {
	profiles := make(map[string]catalog.ResourceProfile, len(resources))
	serviceKeys := map[string]string{}
	for _, profile := range resources {
		if err := indexResourceProfile(profiles, serviceKeys, profile); err != nil {
			return nil, err
		}
	}
	return profiles, nil
}

func indexResourceProfile(
	profiles map[string]catalog.ResourceProfile,
	serviceKeys map[string]string,
	profile catalog.ResourceProfile,
) error {
	if _, ok := profiles[profile.Service]; ok {
		return catalogVerificationError(
			"catalog contains duplicate resource profiles",
			"refresh the catalog and retry",
			fmt.Errorf("duplicate resource service %q", profile.Service),
		)
	}
	key := serviceKey(profile.Service)
	if key == "" {
		return catalogVerificationError(
			"catalog contains an invalid resource service",
			"refresh the catalog and retry",
			fmt.Errorf("service %q derives an empty service key", profile.Service),
		)
	}
	if other, ok := serviceKeys[key]; ok {
		return catalogVerificationError(
			"catalog contains colliding resource service keys",
			"refresh the catalog and retry",
			fmt.Errorf("services %q and %q both derive %q", other, profile.Service, key),
		)
	}
	serviceKeys[key] = profile.Service
	profiles[profile.Service] = profile
	return nil
}

func (p *installPlan) chooseMinimumResourceProfile(
	req types.InstallRequest,
	host system.HostResources,
	onProgress types.ProgressFn,
) (bool, error) {
	availableMemory, availableCPU := installResourceGuidanceBudget(host)
	recMemory := p.recommendedResources.MemoryBytes
	recCPU := p.recommendedResources.CPUs
	useMin := req.ResourceProfile == types.ResourceProfileMin
	if !useMin {
		useMin = recMemory > availableMemory || recCPU > availableCPU
	}
	if !useMin {
		return false, nil
	}
	if _, _, err := sumResourceBand(p.app.Resources, types.ResourceProfileMin); err != nil {
		return false, err
	}
	if req.ResourceProfile != types.ResourceProfileMin && onProgress != nil {
		onProgress(types.StepInstallResourceDegraded, 15, "using minimum resource profile")
	}
	return true, nil
}

func selectResourceProfiles(
	resources []catalog.ResourceProfile,
	useMin bool,
) map[string]selectedResource {
	selected := make(map[string]selectedResource, len(resources))
	for _, profile := range resources {
		chosen := selectedResource{
			memory: profile.Memory.Recommended,
			cpus:   profile.CPUs.Recommended,
			pids:   profile.PIDs.Default,
		}
		if useMin {
			chosen.memory = profile.Memory.Min
			chosen.cpus = profile.CPUs.Min
		}
		selected[profile.Service] = chosen
	}
	return selected
}

func applyResourceOverrides(
	selected map[string]selectedResource,
	profiles map[string]catalog.ResourceProfile,
	overrides []types.ResourceOverride,
) error {
	for _, override := range overrides {
		if err := applyResourceOverride(selected, profiles, override); err != nil {
			return err
		}
	}
	return nil
}

func applyResourceOverride(
	selected map[string]selectedResource,
	profiles map[string]catalog.ResourceProfile,
	override types.ResourceOverride,
) error {
	profile, ok := profiles[override.Service]
	if !ok {
		return usageValidationError(
			"resource override targets an unknown service",
			"choose a service declared by the selected app",
			fmt.Errorf("unknown service %q", override.Service),
		)
	}
	if !profile.AllowOverride {
		return usageValidationError(
			"resource override is not allowed for this service",
			"remove the resource override for this service",
			fmt.Errorf("service %q disallows overrides", override.Service),
		)
	}
	chosen := selected[override.Service]
	var err error
	chosen, err = applyResourceLimitOverride(chosen, profile, override)
	if err != nil {
		return err
	}
	selected[override.Service] = chosen
	return nil
}

func applyResourceLimitOverride(
	chosen selectedResource,
	profile catalog.ResourceProfile,
	override types.ResourceOverride,
) (selectedResource, error) {
	if override.Memory != "" {
		if err := validateMemoryOverride(profile, override.Memory); err != nil {
			return selectedResource{}, err
		}
		chosen.memory = override.Memory
	}
	if override.CPUs != "" {
		if err := validateCPUOverride(profile, override.CPUs); err != nil {
			return selectedResource{}, err
		}
		chosen.cpus = override.CPUs
	}
	if override.PIDs != 0 {
		if err := validatePIDsOverride(profile, override.PIDs); err != nil {
			return selectedResource{}, err
		}
		chosen.pids = override.PIDs
	}
	return chosen, nil
}

func validatePIDsOverride(profile catalog.ResourceProfile, pids int) error {
	if pids >= 1 && pids <= profile.PIDs.Max {
		return nil
	}
	return usageValidationError(
		fmt.Sprintf("pids limit must be between 1 and %d", profile.PIDs.Max),
		fmt.Sprintf("choose a pids value between 1 and %d for %s", profile.PIDs.Max, profile.Service),
		fmt.Errorf("service %q pids override %d is outside [1,%d]", profile.Service, pids, profile.PIDs.Max),
	)
}

func (p *installPlan) addSelectedResourceValues(selected map[string]selectedResource) error {
	for _, profile := range p.app.Resources {
		key := serviceKey(profile.Service)
		chosen := selected[profile.Service]
		if err := p.addSyntheticResolvedValue("MEMORY_LIMIT_"+key, chosen.memory); err != nil {
			return err
		}
		if err := p.addSyntheticResolvedValue("CPUS_LIMIT_"+key, chosen.cpus); err != nil {
			return err
		}
		if err := p.addSyntheticResolvedValue("PIDS_LIMIT_"+key, strconv.Itoa(chosen.pids)); err != nil {
			return err
		}
	}
	return nil
}

type selectedResource struct {
	memory string
	cpus   string
	pids   int
}

func installResourceGuidanceBudget(host system.HostResources) (uint64, float64) {
	memoryBudget := uint64(0)
	if host.TotalMemoryBytes > installHostMemoryReserveBytes {
		memoryBudget = host.TotalMemoryBytes - installHostMemoryReserveBytes
	}
	return memoryBudget, float64(host.CPUCores)
}

func sumResourceBand(resources []catalog.ResourceProfile, profile types.ResourceProfile) (uint64, float64, error) {
	var memory uint64
	var cpus float64
	for _, resource := range resources {
		var memText, cpuText string
		switch profile {
		case types.ResourceProfileRecommended:
			memText = resource.Memory.Recommended
			cpuText = resource.CPUs.Recommended
		case types.ResourceProfileMin:
			memText = resource.Memory.Min
			cpuText = resource.CPUs.Min
		default:
			return 0, 0, usageValidationError(
				"resource profile is invalid",
				"choose recommended or min",
				fmt.Errorf("unknown resource profile %q", profile),
			)
		}
		memBytes, err := parseMemoryBytes(memText)
		if err != nil {
			return 0, 0, catalogVerificationError(
				"catalog contains an invalid memory limit",
				"refresh the catalog and retry",
				err,
			)
		}
		cpuValue, err := strconv.ParseFloat(cpuText, 64)
		if err != nil || cpuValue <= 0 {
			return 0, 0, catalogVerificationError(
				"catalog contains an invalid cpu limit",
				"refresh the catalog and retry",
				fmt.Errorf("cpu limit %q is invalid", cpuText),
			)
		}
		memory += memBytes
		cpus += cpuValue
	}
	return memory, cpus, nil
}

func validateMemoryOverride(profile catalog.ResourceProfile, value string) error {
	got, err := parseMemoryBytes(value)
	if err != nil {
		return usageValidationError(
			"memory override is invalid",
			"pass a Docker memory value such as 512m or 1g",
			err,
		)
	}
	minValue, err := parseMemoryBytes(profile.Memory.Min)
	if err != nil {
		return catalogVerificationError("catalog contains an invalid memory limit", "refresh the catalog and retry", err)
	}
	maxValue, err := parseMemoryBytes(profile.Memory.Max)
	if err != nil {
		return catalogVerificationError("catalog contains an invalid memory limit", "refresh the catalog and retry", err)
	}
	if got < minValue || got > maxValue {
		return usageValidationError(
			fmt.Sprintf("memory limit must be between %s and %s", profile.Memory.Min, profile.Memory.Max),
			fmt.Sprintf("choose memory between %s and %s for %s", profile.Memory.Min, profile.Memory.Max, profile.Service),
			fmt.Errorf("service %q memory override %q outside [%s,%s]", profile.Service, value, profile.Memory.Min, profile.Memory.Max),
		)
	}
	return nil
}

func validateCPUOverride(profile catalog.ResourceProfile, value string) error {
	got, err := strconv.ParseFloat(value, 64)
	if err != nil || got <= 0 {
		return usageValidationError(
			"cpu override is invalid",
			"pass a positive decimal CPU value such as 0.5 or 1.0",
			fmt.Errorf("cpu override %q is invalid", value),
		)
	}
	minValue, err := strconv.ParseFloat(profile.CPUs.Min, 64)
	if err != nil {
		return catalogVerificationError("catalog contains an invalid cpu limit", "refresh the catalog and retry", err)
	}
	maxValue, err := strconv.ParseFloat(profile.CPUs.Max, 64)
	if err != nil {
		return catalogVerificationError("catalog contains an invalid cpu limit", "refresh the catalog and retry", err)
	}
	if got < minValue || got > maxValue {
		return usageValidationError(
			fmt.Sprintf("cpus limit must be between %s and %s", profile.CPUs.Min, profile.CPUs.Max),
			fmt.Sprintf("choose cpus between %s and %s for %s", profile.CPUs.Min, profile.CPUs.Max, profile.Service),
			fmt.Errorf("service %q cpu override %q outside [%s,%s]", profile.Service, value, profile.CPUs.Min, profile.CPUs.Max),
		)
	}
	return nil
}

func parseMemoryBytes(value string) (uint64, error) {
	if value == "" {
		return 0, fmt.Errorf("memory value must not be empty")
	}
	unit := value[len(value)-1]
	multiplier := uint64(1)
	var number string
	switch unit {
	case 'b':
		number = value[:len(value)-1]
	case 'k':
		number = value[:len(value)-1]
		multiplier = 1024
	case 'm':
		number = value[:len(value)-1]
		multiplier = 1024 * 1024
	case 'g':
		number = value[:len(value)-1]
		multiplier = 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("memory value %q must end in b, k, m, or g", value)
	}
	amount, err := strconv.ParseUint(number, 10, 64)
	if err != nil || amount == 0 {
		return 0, fmt.Errorf("memory value %q is invalid", value)
	}
	if amount > math.MaxUint64/multiplier {
		return 0, fmt.Errorf("memory value %q overflows uint64", value)
	}
	return amount * multiplier, nil
}
