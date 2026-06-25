package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
	"gopkg.in/yaml.v3"
)

// validateInstallComposeConfig runs `docker compose config --quiet`
// against a private 0o700 tempdir copy of the complete rendered artifact
// set (PRD §13), so validation happens
// before any byte is exposed under the stack path. The secret-bearing
// copies keep secret-file mode through the same atomic write path as the
// real stack write; the workspace is removed best-effort on return.
// Client errors propagate unchanged so the internal/docker error-code
// mapping stays authoritative.
func validateInstallComposeConfig(
	ctx context.Context,
	client docker.Client,
	plan *installPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepInstallComposeValidate, 30, "validating compose config")
	}
	return validateRenderedComposeConfig(ctx, client, &plan.rendered)
}

// validateRenderedComposeConfig runs `docker compose config --quiet`
// against a private 0o700 tempdir copy of the COMPLETE deployed artifact
// set — docker-compose.yml, .env, every additional_file, and every
// config_generation artifact (PRD §13). Staging the full set (not just
// compose + env) keeps the validated layout faithful to what deploys: an
// `env_file:` pointing at a rendered config artifact, for example,
// resolves against the temp project dir exactly as it will against the
// stack. The same [renderedArtifactWrites] enumerator backs both this
// staging and the real install writer, so the two cannot drift.
// Both the install pre-exposure validation and the update post-rewrite
// validation (PRD §20 step 9) share it: each validates the in-memory
// rendered bytes hermetically rather than the live stack files, so the
// secret-bearing copies never outlive the call. Secret-bearing copies
// keep secret-file mode through the same atomic write path as the real
// stack write; the workspace is removed best-effort on return. Client
// errors propagate unchanged so the internal/docker error-code mapping
// stays authoritative.
func validateRenderedComposeConfig(
	ctx context.Context,
	client docker.Client,
	rendered *render.RenderedStack,
) error {
	if rendered == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"rendered stack is required",
			"render the stack before validating compose config",
		)
	}

	tempDir, err := os.MkdirTemp("", "wdm-compose-validate-*")
	if err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"compose validation workspace could not be created",
			"check temp directory permissions and retry",
			err,
		)
	}
	defer os.RemoveAll(tempDir) //nolint:errcheck // best-effort cleanup; the 0o700 workspace and any 0o600 copies stay private regardless

	// Stage exactly what the real writer deploys: compose + .env first,
	// then the rendered additional_files and config_artifacts rooted at
	// the temp dir via the shared enumerator. The whole set is removed
	// with the workspace on return.
	composePath := filepath.Join(tempDir, installComposeFilename)
	staged := []installFileWrite{
		{path: composePath, data: rendered.ComposeBytes, mode: installComposeFileMode},
		{path: filepath.Join(tempDir, installEnvFilename), data: rendered.EnvBytes, mode: security.SecretFileMode},
		// Stage an empty .env.user so a template's env_file: [.env.user]
		// resolves during `compose config`. It is user-owned, not a
		// rendered artifact, so it is seeded on disk at install (T2) and
		// staged empty here for pre-write validation only.
		{path: filepath.Join(tempDir, installEnvUserFilename), data: []byte{}, mode: security.SecretFileMode},
	}
	artifactWrites, err := renderedArtifactWrites(rendered, tempDir)
	if err != nil {
		return err
	}
	staged = append(staged, artifactWrites...)

	for _, write := range staged {
		// Create nested parents (e.g. init-scripts/) at 0o700 inside the
		// 0o700 workspace before the atomic write, so a secret-mode copy's
		// parent is never group/world-writable.
		if err := os.MkdirAll(filepath.Dir(write.path), 0o700); err != nil {
			return types.WrapError(
				types.ErrCodeGeneric,
				"compose validation copy could not be written",
				"check temp directory permissions and retry",
				err,
			)
		}
		if err := state.WriteFileAtomic(write.path, write.data, write.mode); err != nil {
			return types.WrapError(
				types.ErrCodeGeneric,
				"compose validation copy could not be written",
				"check temp directory permissions and retry",
				err,
			)
		}
	}
	return docker.ValidateComposeConfig(ctx, client, tempDir, composePath)
}

func verifyRenderedNonSecretArtifacts(
	redactor security.Redactor,
	secrets []string,
	stack render.RenderedStack,
	guidance *types.PostInstallGuidance,
) error {
	artifacts := []struct {
		name  string
		bytes []byte
	}{
		{name: "docker-compose.yml", bytes: stack.ComposeBytes},
		{name: "post-install guidance", bytes: guidanceText(guidance)},
	}
	for _, file := range stack.AdditionalFiles {
		if file.Mode == "0600" {
			continue
		}
		artifacts = append(artifacts, struct {
			name  string
			bytes []byte
		}{
			name:  file.Dest,
			bytes: file.Bytes,
		})
	}
	// Config artifacts share the additional_files convention: 0600 is the
	// secret-bearing mode and is excluded, but any non-0600 config artifact
	// is a non-secret sink and must be refused if it carries a generated or
	// reused secret (PRD §17, §24).
	for _, artifact := range stack.ConfigArtifacts {
		if artifact.Mode == "0600" {
			continue
		}
		artifacts = append(artifacts, struct {
			name  string
			bytes []byte
		}{
			name:  artifact.Dest,
			bytes: artifact.Bytes,
		})
	}

	for _, artifact := range artifacts {
		for _, secret := range secrets {
			if secret == "" || !bytes.Contains(artifact.bytes, []byte(secret)) {
				continue
			}
			return redactedVerificationError(
				redactor,
				"rendered non-secret artifact contains a generated secret",
				"fix the catalog template so generated secrets stay in .env or 0600-only files",
				fmt.Errorf("%s contains generated secret %q", artifact.name, secret),
			)
		}
	}
	return nil
}

// composeImageProjection is the minimal slice of a rendered
// docker-compose.yml needed to read each service's image: literal.
// Only the image field is decoded; yaml.v3 ignores every other service
// key because the struct declares no field for it.
type composeImageProjection struct {
	Services map[string]struct {
		Image string `yaml:"image"`
	} `yaml:"services"`
}

// verifyImagePinsMatchTemplate enforces that every catalog image pin
// names the exact image[:tag] the rendered Compose service deploys
// (PRD §9, §22). The catalog pins drive wdm's update diffing
// (diffUpdateServicePins compares lock pins against catalog pins); the
// template's literal image: line drives what `docker compose up`
// actually pulls. When the two drift — a maintainer bumps the template
// image without the pin, or vice versa — wdm would report "updates"
// that do not match what deploys, a silent correctness lie. This check
// makes that drift structurally impossible by refusing install and
// update before any Docker contact.
// It parses the RENDERED compose bytes
// ([render.RenderedStack.ComposeBytes]) rather than the raw template
// text: the rendered output is genuine YAML already emitted by
// [render.RenderLabels], so the parse is robust against any
// text/template actions the template carried and inspects the exact
// image references Compose will see. The compared reference is
// `image[:tag]` built from the catalog pin via [updateImageRef] —
// byte-identical to the update path's old → new diff surface — so the
// install-time and update-time views of "what is pinned" cannot
// diverge either.
// Both the install render path ([Engine.renderInstall]) and the update
// re-render path (rewriteUpdateStack) call this alongside
// [verifyRenderedNonSecretArtifacts], so a drifted catalog is refused
// on both arcs. Failures wrap [types.ErrCodeVerificationFailed] (the
// catalog-integrity class, matching the surrounding render errors)
// through the operation redactor and name the app, service, pinned
// image, and rendered template image. Catalog metadata carries no
// secrets, but the cause is redacted defensively for parity with the
// sibling render-stage errors.
func verifyImagePinsMatchTemplate(redactor security.Redactor, app catalog.App, composeBytes []byte) error {
	var projection composeImageProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for image-pin verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	seen := map[string]struct{}{}
	for _, pin := range app.ImagePins {
		if pin.Service == "" {
			continue
		}
		if _, ok := seen[pin.Service]; ok {
			continue
		}
		seen[pin.Service] = struct{}{}

		pinnedRef := updateImageRef(pin.Image, pin.Tag)
		service, ok := projection.Services[pin.Service]
		if !ok {
			return redactedVerificationError(
				redactor,
				"catalog image pin names a service absent from the rendered compose",
				"align the catalog image_pins service names with the compose template",
				fmt.Errorf(
					"app %q pins image %q for service %q but the rendered compose declares no such service",
					app.AppID,
					pinnedRef,
					pin.Service,
				),
			)
		}
		if service.Image != pinnedRef {
			return redactedVerificationError(
				redactor,
				"catalog image pin does not match the rendered compose image",
				"align the catalog image_pins tag with the compose template image line (or vice versa)",
				fmt.Errorf(
					"app %q service %q: catalog pins image %q but the compose template deploys %q",
					app.AppID,
					pin.Service,
					pinnedRef,
					service.Image,
				),
			)
		}
	}
	return nil
}

// composePortsProjection is the minimal slice of a rendered
// docker-compose.yml needed to read each service's published host-port
// bindings and network mode. Only services.<name>.ports and
// services.<name>.network_mode are decoded; yaml.v3 ignores every other key
// because the struct declares no field for it.
type composePortsProjection struct {
	Services map[string]composePortsService `yaml:"services"`
}

// composePortsService carries one service's rendered ports and network mode.
// network_mode is read so the public-bind scan can refuse host networking,
// which exposes every container port outside the services.<name>.ports list
// and would otherwise be invisible to the scan.
type composePortsService struct {
	Ports       []composePortEntry `yaml:"ports"`
	NetworkMode string             `yaml:"network_mode"`
}

// composePortEntry is one rendered Compose ports: entry. Compose accepts
// both the short string form ("127.0.0.1:3008:3001", "6881:6881/udp") and
// the long mapping form ({target, published, host_ip, protocol}); a custom
// UnmarshalYAML normalizes both into the same fields so the public-bind scan
// reads one shape.
type composePortEntry struct {
	hostIP    string
	published string
	protocol  string
	raw       string
}

type composePortLong struct {
	HostIP    string `yaml:"host_ip"`
	Published string `yaml:"published"`
	Target    string `yaml:"target"`
	Protocol  string `yaml:"protocol"`
}

// UnmarshalYAML accepts either a scalar short form or a mapping long form.
// The short form is parsed lexically rather than expanded: only the host IP,
// the published host port (or range), and the protocol matter to the
// public-bind classification.
func (e *composePortEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		e.raw = node.Value
		e.hostIP, e.published, e.protocol = parseShortPort(node.Value)
		return nil
	case yaml.MappingNode:
		var long composePortLong
		if err := node.Decode(&long); err != nil {
			return err
		}
		published := long.Published
		if published == "" {
			published = long.Target
		}
		e.hostIP = long.HostIP
		e.published = published
		e.protocol = long.Protocol
		e.raw = fmt.Sprintf("%s:%s/%s", long.HostIP, published, long.Protocol)
		return nil
	default:
		return fmt.Errorf("unexpected compose port node kind %d", node.Kind)
	}
}

// parseShortPort splits a Compose short-form port string into host IP,
// published host port (or range), and protocol. Short form is
// "[host_ip:][host:]container[/protocol]"; the host IP, when present, is the
// leading segment that is not purely numeric/range. A bare "6881:6881" has
// no host IP, so Docker would bind all interfaces — that is why an empty host
// IP classifies as public downstream.
func parseShortPort(value string) (hostIP, published, protocol string) {
	spec, proto, hasProto := strings.Cut(value, "/")
	if hasProto {
		protocol = proto
	}
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 3:
		// host_ip:host:container
		return parts[0], parts[1], protocol
	case 2:
		// host:container (no host IP, binds all interfaces)
		return "", parts[0], protocol
	default:
		// container-only (Docker assigns the host port, all interfaces)
		return "", spec, protocol
	}
}

// verifyPublicBindsMatchCatalog enforces PRD §11.1(a)(b): the set of PUBLIC
// host-port binds in the rendered compose must exactly equal the set of
// port.public:true declarations in the signed catalog, matched by (protocol,
// host port / host range). A published port is local IFF its host IP is a
// loopback address; 0.0.0.0, an empty/missing host IP (Docker defaults to all
// interfaces), or any non-loopback IP is PUBLIC and requires a backing public
// declaration. This makes two failures structurally impossible before any
// Docker contact: an unsigned/tampered template introducing a public bind
// with no catalog backing (§11.1(b)), and a public declaration that renders
// as 127.0.0.1 so the warning would lie. It also refuses any service that
// renders network_mode: host, which publishes every container port on the
// host outside the scanned ports list and would otherwise hide exposure from
// the scan — host networking is never permitted in the curated set. Both
// render paths ([Engine.renderInstall] and rewriteUpdateStack) call it
// alongside [verifyImagePinsMatchTemplate]. Failures wrap
// [types.ErrCodeVerificationFailed] through the operation redactor;
// catalog/template metadata carries no secrets but is redacted defensively
// for parity with the sibling render errors.
func verifyPublicBindsMatchCatalog(redactor security.Redactor, app catalog.App, composeBytes []byte) error {
	var projection composePortsProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for public-bind verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	// Host networking publishes every container port on the host directly,
	// bypassing the services.<name>.ports list the public-bind scan reads, so
	// the scan can never see that exposure. It is never permitted in the
	// curated set; refuse any service that renders it (fail closed).
	for _, name := range sortedServiceNames(projection) {
		if strings.EqualFold(strings.TrimSpace(projection.Services[name].NetworkMode), "host") {
			return redactedVerificationError(
				redactor,
				"rendered compose runs a service on the host network",
				"remove network_mode: host; bind only the declared ports on 127.0.0.1 (or a declared public port)",
				fmt.Errorf(
					"app %q service %q renders network_mode: host, which exposes every container port outside the scanned ports list",
					app.AppID,
					name,
				),
			)
		}
	}

	declared, err := declaredPublicBinds(app)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"catalog public port declaration is invalid",
			"refresh the catalog and retry",
			err,
		)
	}

	rendered, err := renderedPublicBinds(app, projection)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose public bind could not be classified",
			"refresh the catalog and retry",
			err,
		)
	}

	// A public bind in the rendered compose with no backing public:true
	// declaration is an unsigned/tampered template introducing exposure.
	for _, key := range sortedBindKeys(rendered) {
		if _, ok := declared[key]; !ok {
			return redactedVerificationError(
				redactor,
				"rendered compose binds a public port the catalog does not declare",
				"a public bind must be declared public:true in the signed catalog",
				fmt.Errorf(
					"app %q service %q binds public port %s with no catalog public declaration",
					app.AppID,
					rendered[key],
					key,
				),
			)
		}
	}
	// A public:true declaration that did not render as a public bind means
	// the template drifted to 127.0.0.1; the warning would otherwise lie.
	for _, key := range sortedBindKeys(declared) {
		if _, ok := rendered[key]; !ok {
			return redactedVerificationError(
				redactor,
				"catalog declares a public port the rendered compose does not bind publicly",
				"align the compose template bind interface with the catalog public declaration",
				fmt.Errorf(
					"app %q service %q declares public port %s but the rendered compose does not bind it on all interfaces",
					app.AppID,
					declared[key],
					key,
				),
			)
		}
	}
	return nil
}

// declaredPublicBinds maps each catalog public:true port to its service,
// keyed by "<protocol>/<host>" (single ports) or "<protocol>/<lo>-<hi>"
// (ranges). The same protocol/range normalization is applied on the rendered
// side so the two sets compare like for like.
func declaredPublicBinds(app catalog.App) (map[string]string, error) {
	declared := map[string]string{}
	for _, port := range app.Ports {
		if !port.Public {
			continue
		}
		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		if port.HostRange != "" {
			lo, hi, err := parsePortRange(port.Service, port.HostRange)
			if err != nil {
				return nil, err
			}
			declared[fmt.Sprintf("%s/%d-%d", protocol, lo, hi)] = port.Service
			continue
		}
		declared[fmt.Sprintf("%s/%d", protocol, port.Host)] = port.Service
	}
	return declared, nil
}

// renderedPublicBinds maps each PUBLIC published bind in the rendered compose
// to its service, keyed the same way as declaredPublicBinds. Loopback-bound
// ports are local and excluded; everything else is public per the fail-closed
// classification.
func renderedPublicBinds(
	app catalog.App,
	projection composePortsProjection,
) (map[string]string, error) {
	rendered := map[string]string{}
	for _, service := range sortedServiceNames(projection) {
		for _, entry := range projection.Services[service].Ports {
			if isLoopbackBind(entry.hostIP) {
				continue
			}
			protocol := entry.protocol
			if protocol == "" {
				protocol = "tcp"
			}
			key, err := publicBindKey(protocol, entry)
			if err != nil {
				return nil, fmt.Errorf("app %q service %q: %w", app.AppID, service, err)
			}
			rendered[key] = service
		}
	}
	return rendered, nil
}

// publicBindKey normalizes a rendered published value (a single port or a
// "lo-hi" range) into the comparison key shared with the catalog side.
func publicBindKey(protocol string, entry composePortEntry) (string, error) {
	if entry.published == "" {
		return "", fmt.Errorf("public bind %q has no published host port", entry.raw)
	}
	if lo, hi, ok := strings.Cut(entry.published, "-"); ok {
		loPort, loErr := strconv.Atoi(lo)
		hiPort, hiErr := strconv.Atoi(hi)
		if loErr != nil || hiErr != nil {
			return "", fmt.Errorf("public bind %q has a non-numeric port range", entry.raw)
		}
		return fmt.Sprintf("%s/%d-%d", protocol, loPort, hiPort), nil
	}
	port, err := strconv.Atoi(entry.published)
	if err != nil {
		return "", fmt.Errorf("public bind %q has a non-numeric host port", entry.raw)
	}
	return fmt.Sprintf("%s/%d", protocol, port), nil
}

// isLoopbackBind reports whether a Compose host_ip binds a loopback address
// (127.0.0.0/8 or ::1). An empty host IP is NOT loopback: Docker defaults a
// portless or IP-less binding to all interfaces, so it must classify as
// public (fail closed).
func isLoopbackBind(hostIP string) bool {
	if hostIP == "" {
		return false
	}
	ip := net.ParseIP(hostIP)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func sortedServiceNames(projection composePortsProjection) []string {
	names := make([]string, 0, len(projection.Services))
	for name := range projection.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedBindKeys(binds map[string]string) []string {
	keys := make([]string, 0, len(binds))
	for key := range binds {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// allowedCapabilities is the closed Linux-capability allow-list from PRD §12.2.
// SYS_MODULE is intentionally excluded: a kernel-module need is met by a host
// modprobe prerequisite plus a /lib/modules:ro mount, not the capability. Keys
// are the bare, upper-case capability names (no CAP_ prefix); the scan
// normalizes rendered values before matching.
var allowedCapabilities = map[string]struct{}{
	"NET_BIND_SERVICE": {},
	"CHOWN":            {},
	"SETUID":           {},
	"SETGID":           {},
	"DAC_OVERRIDE":     {},
	"FOWNER":           {},
	"NET_ADMIN":        {},
	"NET_RAW":          {},
}

// allowedSysctls is the closed sysctl allow-list from PRD §12.2.
var allowedSysctls = map[string]struct{}{
	"net.ipv4.ip_forward":              {},
	"net.ipv4.conf.all.src_valid_mark": {},
}

// composePrivilegeProjection is the minimal slice of a rendered
// docker-compose.yml needed to read each service's container-privilege posture.
// Only the cap_add/cap_drop/sysctls/devices/privileged keys are decoded; yaml.v3
// ignores every other key because the struct declares no field for it.
type composePrivilegeProjection struct {
	Services map[string]composePrivilegeService `yaml:"services"`
}

// composePrivilegeService carries one service's rendered privilege keys.
// Sysctls is a custom type because Compose accepts both a mapping form
// ({name: value}) and a sequence form ([name=value]); UnmarshalYAML normalizes
// both into the same name→value shape so the scan reads one form.
type composePrivilegeService struct {
	CapAdd     []string       `yaml:"cap_add"`
	CapDrop    []string       `yaml:"cap_drop"`
	Sysctls    composeSysctls `yaml:"sysctls"`
	Devices    []string       `yaml:"devices"`
	Privileged bool           `yaml:"privileged"`
}

// composeSysctls is the rendered sysctls block normalized to name→value.
type composeSysctls struct {
	entries map[string]string
}

// UnmarshalYAML accepts Compose's two sysctls forms: a mapping {name: value}
// and a sequence of "name=value" strings. Any other node kind, or a sequence
// entry without an '=', fails closed so the scan refuses an unclassifiable
// declaration rather than silently passing it.
func (s *composeSysctls) UnmarshalYAML(node *yaml.Node) error {
	s.entries = map[string]string{}
	switch node.Kind {
	case yaml.MappingNode:
		var mapping map[string]string
		if err := node.Decode(&mapping); err != nil {
			return err
		}
		for name, value := range mapping {
			s.entries[name] = value
		}
		return nil
	case yaml.SequenceNode:
		var items []string
		if err := node.Decode(&items); err != nil {
			return err
		}
		for _, item := range items {
			name, value, ok := strings.Cut(item, "=")
			if !ok {
				return fmt.Errorf("sysctl entry %q is not in name=value form", item)
			}
			s.entries[name] = value
		}
		return nil
	default:
		return fmt.Errorf("unexpected compose sysctls node kind %d", node.Kind)
	}
}

// normalizeCapability returns the bare, upper-case capability name used for
// allow-list matching. Compose accepts both "NET_ADMIN" and "CAP_NET_ADMIN"
// and is case-insensitive, so a leading CAP_ prefix is stripped and the name
// upper-cased before comparison.
func normalizeCapability(name string) string {
	upper := strings.ToUpper(strings.TrimSpace(name))
	return strings.TrimPrefix(upper, "CAP_")
}

// verifyContainerPrivilegeMatchCatalog enforces the PRD §12.2 closed
// container-privilege allow-list against the rendered compose, mirroring the
// public-bind scan ([verifyPublicBindsMatchCatalog]). It runs three checks and
// fails closed on any YAML it cannot classify:
//
//   - (A) the catalog's per-service ServiceHardening declarations must stay
//     inside the allow-list (caps, sysctls, empty device set, privileged=false);
//   - (B) every rendered service — declaring or not — must keep cap_add inside
//     the allow-list, keep sysctls inside the allow-list, declare no devices,
//     run unprivileged, and (when it adds any capability) carry cap_drop:ALL as
//     the baseline; and a service the catalog does NOT declare must add zero
//     capabilities and set zero sysctls — any elevation a non-declaring service
//     renders is unbacked by the signed catalog and is refused; and
//   - (C) every service that HAS a ServiceHardening entry must render exactly
//     the declared capability and sysctl sets (defense-in-depth parity for the
//     hardened apps). A non-declaring service's zero-elevation posture is the
//     baseline (enforced by (B)), so it is not parity-checked. This keeps the
//     four cap-using curated apps green via parity and the zero-cap apps green
//     via the baseline.
//
// Both render paths ([Engine.renderInstall] and rewriteUpdateStack) call it
// alongside [verifyPublicBindsMatchCatalog]. Catalog-declaration refusals (A)
// name only allow-list metadata (capability/sysctl names are not secrets) and
// use [catalogVerificationError]; every refusal derived from the rendered
// compose, and any parse failure, is routed through [redactedVerificationError]
// so rendered content can never leak. All failures map to
// [types.ErrCodeVerificationFailed].
func verifyContainerPrivilegeMatchCatalog(
	redactor security.Redactor,
	app catalog.App,
	composeBytes []byte,
) error {
	if err := verifyCatalogPrivilegeDeclarations(app); err != nil {
		return err
	}

	var projection composePrivilegeProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for container-privilege verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	declaredServices := declaredHardeningServices(app)
	if err := verifyRenderedPrivilegeBounds(redactor, app, projection, declaredServices); err != nil {
		return err
	}
	return verifyRenderedPrivilegeParity(redactor, app, projection)
}

// declaredHardeningServices is the set of Compose service names the catalog
// declares a ServiceHardening entry for. A service outside this set must render
// the zero-elevation baseline (no cap_add, no sysctls); a service inside it is
// governed by the parity check.
func declaredHardeningServices(app catalog.App) map[string]struct{} {
	declared := make(map[string]struct{}, len(app.ServiceHardening))
	for _, hardening := range app.ServiceHardening {
		declared[hardening.Service] = struct{}{}
	}
	return declared
}

// verifyCatalogPrivilegeDeclarations enforces check (A): every
// ServiceHardening entry must stay inside the closed allow-list. It reads only
// catalog metadata, so refusals are non-redacting.
func verifyCatalogPrivilegeDeclarations(app catalog.App) error {
	for _, hardening := range app.ServiceHardening {
		if hardening.Capabilities != nil {
			for _, capability := range hardening.Capabilities.Add {
				if _, ok := allowedCapabilities[normalizeCapability(capability)]; !ok {
					return catalogVerificationError(
						"catalog declares a capability outside the allow-list",
						"declare only PRD §12.2 allow-list capabilities, or drop the capability",
						fmt.Errorf(
							"app %q service %q declares capability %q outside the allow-list",
							app.AppID,
							hardening.Service,
							capability,
						),
					)
				}
			}
		}
		for _, sysctl := range hardening.Sysctls {
			if _, ok := allowedSysctls[sysctl.Name]; !ok {
				return catalogVerificationError(
					"catalog declares a sysctl outside the allow-list",
					"declare only PRD §12.2 allow-list sysctls, or drop the sysctl",
					fmt.Errorf(
						"app %q service %q declares sysctl %q outside the allow-list",
						app.AppID,
						hardening.Service,
						sysctl.Name,
					),
				)
			}
		}
		if len(hardening.Devices) > 0 {
			return catalogVerificationError(
				"catalog declares a device map but the device allow-list is empty",
				"remove the device declaration",
				fmt.Errorf(
					"app %q service %q declares %d device(s) against an empty allow-list",
					app.AppID,
					hardening.Service,
					len(hardening.Devices),
				),
			)
		}
		if hardening.Privileged {
			return catalogVerificationError(
				"catalog declares a privileged service absent a recorded amendment",
				"keep privileged false; a privileged declaration requires a recorded PRD amendment",
				fmt.Errorf(
					"app %q service %q declares privileged:true",
					app.AppID,
					hardening.Service,
				),
			)
		}
	}
	return nil
}

// verifyRenderedPrivilegeBounds enforces check (B): the universal allow-list
// bounds that apply to every rendered service whether or not the catalog
// declares hardening for it, plus the requirement that a service the catalog
// does NOT declare renders the zero-elevation baseline (no cap_add, no
// sysctls). declaredServices is the set of service names with a catalog
// ServiceHardening entry; a declaring service's caps/sysctls are governed by
// the parity check instead. Refusals reference rendered compose content, so
// they are redacted.
func verifyRenderedPrivilegeBounds(
	redactor security.Redactor,
	app catalog.App,
	projection composePrivilegeProjection,
	declaredServices map[string]struct{},
) error {
	for _, name := range sortedPrivilegeServiceNames(projection) {
		service := projection.Services[name]
		if _, declared := declaredServices[name]; !declared {
			if len(service.CapAdd) > 0 {
				return redactedVerificationError(
					redactor,
					"rendered compose adds a capability the catalog does not declare",
					"declare the capability in catalog service_hardening or remove it from the compose template",
					fmt.Errorf(
						"app %q service %q adds capabilities but the catalog declares no service_hardening for it",
						app.AppID,
						name,
					),
				)
			}
			if len(service.Sysctls.entries) > 0 {
				return redactedVerificationError(
					redactor,
					"rendered compose sets a sysctl the catalog does not declare",
					"declare the sysctl in catalog service_hardening or remove it from the compose template",
					fmt.Errorf(
						"app %q service %q sets sysctls but the catalog declares no service_hardening for it",
						app.AppID,
						name,
					),
				)
			}
		}
		for _, capability := range service.CapAdd {
			if _, ok := allowedCapabilities[normalizeCapability(capability)]; !ok {
				return redactedVerificationError(
					redactor,
					"rendered compose adds a capability outside the allow-list",
					"a re-added capability must be in the PRD §12.2 allow-list",
					fmt.Errorf(
						"app %q service %q adds capability %q outside the allow-list",
						app.AppID,
						name,
						capability,
					),
				)
			}
		}
		for sysctlName := range service.Sysctls.entries {
			if _, ok := allowedSysctls[sysctlName]; !ok {
				return redactedVerificationError(
					redactor,
					"rendered compose sets a sysctl outside the allow-list",
					"a sysctl must be in the PRD §12.2 allow-list",
					fmt.Errorf(
						"app %q service %q sets sysctl %q outside the allow-list",
						app.AppID,
						name,
						sysctlName,
					),
				)
			}
		}
		if len(service.Devices) > 0 {
			return redactedVerificationError(
				redactor,
				"rendered compose declares a device map but the device allow-list is empty",
				"remove the device mapping from the compose template",
				fmt.Errorf(
					"app %q service %q declares %d device(s) against an empty allow-list",
					app.AppID,
					name,
					len(service.Devices),
				),
			)
		}
		if service.Privileged {
			return redactedVerificationError(
				redactor,
				"rendered compose runs a service privileged",
				"remove privileged:true from the compose template",
				fmt.Errorf(
					"app %q service %q renders privileged:true",
					app.AppID,
					name,
				),
			)
		}
		if len(service.CapAdd) > 0 && !capDropContainsAll(service.CapDrop) {
			return redactedVerificationError(
				redactor,
				"rendered compose adds capabilities without the cap_drop:ALL baseline",
				"keep cap_drop: [ALL] as the baseline and re-add only the declared capabilities",
				fmt.Errorf(
					"app %q service %q adds capabilities but does not drop ALL",
					app.AppID,
					name,
				),
			)
		}
	}
	return nil
}

// verifyRenderedPrivilegeParity enforces check (C): every service the catalog
// hardens must render exactly the declared capability and sysctl sets and the
// declared privileged flag. Refusals reference rendered compose content, so
// they are redacted.
func verifyRenderedPrivilegeParity(
	redactor security.Redactor,
	app catalog.App,
	projection composePrivilegeProjection,
) error {
	for _, hardening := range app.ServiceHardening {
		service, ok := projection.Services[hardening.Service]
		if !ok {
			return redactedVerificationError(
				redactor,
				"catalog hardens a service absent from the rendered compose",
				"align the catalog service_hardening service names with the compose template",
				fmt.Errorf(
					"app %q hardens service %q but the rendered compose declares no such service",
					app.AppID,
					hardening.Service,
				),
			)
		}

		declaredCaps := map[string]struct{}{}
		if hardening.Capabilities != nil {
			for _, capability := range hardening.Capabilities.Add {
				declaredCaps[normalizeCapability(capability)] = struct{}{}
			}
		}
		renderedCaps := map[string]struct{}{}
		for _, capability := range service.CapAdd {
			renderedCaps[normalizeCapability(capability)] = struct{}{}
		}
		if !stringSetsEqual(declaredCaps, renderedCaps) {
			return redactedVerificationError(
				redactor,
				"rendered compose capability set does not match the catalog declaration",
				"align the compose template cap_add with the catalog service_hardening capabilities",
				fmt.Errorf(
					"app %q service %q: catalog declares capabilities %s but the rendered compose adds %s",
					app.AppID,
					hardening.Service,
					sortedSetValues(declaredCaps),
					sortedSetValues(renderedCaps),
				),
			)
		}

		declaredSysctls := map[string]string{}
		for _, sysctl := range hardening.Sysctls {
			declaredSysctls[sysctl.Name] = sysctl.Value
		}
		if !stringMapsEqual(declaredSysctls, service.Sysctls.entries) {
			return redactedVerificationError(
				redactor,
				"rendered compose sysctl set does not match the catalog declaration",
				"align the compose template sysctls with the catalog service_hardening sysctls",
				fmt.Errorf(
					"app %q service %q: catalog declares sysctls %s but the rendered compose sets %s",
					app.AppID,
					hardening.Service,
					sortedSysctlPairs(declaredSysctls),
					sortedSysctlPairs(service.Sysctls.entries),
				),
			)
		}

		if service.Privileged != hardening.Privileged {
			return redactedVerificationError(
				redactor,
				"rendered compose privileged flag does not match the catalog declaration",
				"align the compose template privileged flag with the catalog declaration",
				fmt.Errorf(
					"app %q service %q: catalog declares privileged %t but the rendered compose renders %t",
					app.AppID,
					hardening.Service,
					hardening.Privileged,
					service.Privileged,
				),
			)
		}
	}
	return nil
}

// capDropContainsAll reports whether a rendered cap_drop list drops every
// capability ("ALL", case-insensitive).
func capDropContainsAll(capDrop []string) bool {
	for _, dropped := range capDrop {
		if strings.EqualFold(strings.TrimSpace(dropped), "ALL") {
			return true
		}
	}
	return false
}

func sortedPrivilegeServiceNames(projection composePrivilegeProjection) []string {
	names := make([]string, 0, len(projection.Services))
	for name := range projection.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func stringSetsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if _, ok := b[key]; !ok {
			return false
		}
	}
	return true
}

func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}

func sortedSetValues(set map[string]struct{}) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func sortedSysctlPairs(pairs map[string]string) []string {
	formatted := make([]string, 0, len(pairs))
	for name, value := range pairs {
		formatted = append(formatted, name+"="+value)
	}
	sort.Strings(formatted)
	return formatted
}

// allowedSocketProxyPermissions is the closed set of docker-socket-proxy
// permission flags wdm recognizes (PRD §12.1, schema socket_proxy_permission).
// The read-scoped flags are the baseline; POST is the write/control switch.
var allowedSocketProxyPermissions = map[string]struct{}{
	"CONTAINERS": {}, "IMAGES": {}, "NETWORKS": {}, "VOLUMES": {},
	"INFO": {}, "EVENTS": {}, "PING": {}, "VERSION": {}, "POST": {},
}

// composeSocketProjection is the minimal slice of a rendered docker-compose.yml
// needed to find direct Docker-socket bind mounts. Only services[].volumes is
// decoded; yaml.v3 ignores every other key.
type composeSocketProjection struct {
	Services map[string]composeSocketService `yaml:"services"`
}

type composeSocketService struct {
	Volumes []composeVolume `yaml:"volumes"`
}

// composeVolume captures the host-side source of one Compose volume entry.
// Compose accepts a short string form ("source:target[:mode]") and a long
// mapping form ({source, target, ...}); UnmarshalYAML normalizes both to the
// source string and fails closed on any other node kind so an unclassifiable
// volume is refused rather than silently passed.
type composeVolume struct {
	source string
}

func (v *composeVolume) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var raw string
		if err := node.Decode(&raw); err != nil {
			return err
		}
		source, _, _ := strings.Cut(raw, ":")
		v.source = source
		return nil
	case yaml.MappingNode:
		var mapping struct {
			Source string `yaml:"source"`
		}
		if err := node.Decode(&mapping); err != nil {
			return err
		}
		v.source = mapping.Source
		return nil
	default:
		return fmt.Errorf("unexpected compose volume node kind %d", node.Kind)
	}
}

// normalizeSocketPermission upper-cases and trims a declared permission for
// allow-list matching (the schema enum is upper-case).
func normalizeSocketPermission(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}

// socketProxyAllowsControl reports whether the declared allowed_api includes the
// POST write/control switch (read-and-control vs read-only).
func socketProxyAllowsControl(allowedAPI []string) bool {
	for _, perm := range allowedAPI {
		if normalizeSocketPermission(perm) == "POST" {
			return true
		}
	}
	return false
}

// isDockerSocketSource reports whether a Compose volume host-side source binds
// the Docker socket. It matches the path basename so /var/run/docker.sock,
// /run/docker.sock, and a bare docker.sock all match, while a named volume
// (dockersock) or an unrelated file (docker.sock.conf) do not.
func isDockerSocketSource(source string) bool {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return false
	}
	return path.Base(path.Clean(trimmed)) == "docker.sock"
}

func sortedSocketServiceNames(projection composeSocketProjection) []string {
	names := make([]string, 0, len(projection.Services))
	for name := range projection.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// verifySocketPolicyMatchCatalog enforces PRD §12.1 against the rendered
// compose, mirroring the container-privilege scan. It runs two checks and fails
// closed on any YAML it cannot classify:
//
//   - (A) when the catalog declares a socket_proxy, every allowed_api flag must
//     be in the recognized closed set, the declared network must reference a
//     networks[] entry with internal:true (so wdm creates it --internal), and
//     the proxy service must be image-pinned; and
//   - (B) no rendered service may bind the Docker socket directly, EXCEPT the
//     declared, enabled docker-socket-proxy sidecar, which legitimately mounts
//     it. Every other direct docker.sock bind is a hard failure; and
//   - (C) the declared, enabled proxy sidecar may attach only to internal
//     networks the catalog declares. Check (A) proves the declared
//     socket_proxy.network is internal, but a tampered template could also
//     attach the sidecar to a non-internal (front/egress) network and make the
//     Docker API reachable off-host. Check (C) closes that gap against the
//     rendered compose.
//
// Both render paths (renderInstall and rewriteUpdateStack) call it. Catalog
// declaration refusals (A) name only catalog metadata (permission/network/
// service names are not secrets) and use catalogVerificationError; every refusal
// derived from the rendered compose, and any parse failure, is routed through
// redactedVerificationError. All failures map to types.ErrCodeVerificationFailed.
func verifySocketPolicyMatchCatalog(
	redactor security.Redactor,
	app catalog.App,
	composeBytes []byte,
) error {
	if err := verifyCatalogSocketDeclaration(app); err != nil {
		return err
	}

	var projection composeSocketProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for socket-policy verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}
	if err := verifyRenderedNoDirectSocketMount(redactor, app, projection); err != nil {
		return err
	}
	return verifyRenderedProxyNetworkInternal(redactor, app, composeBytes)
}

// verifyCatalogSocketDeclaration enforces check (A). It reads only catalog
// metadata, so refusals are non-redacting.
func verifyCatalogSocketDeclaration(app catalog.App) error {
	proxy := app.SocketProxy
	if proxy == nil {
		return nil
	}
	for _, perm := range proxy.AllowedAPI {
		if _, ok := allowedSocketProxyPermissions[normalizeSocketPermission(perm)]; !ok {
			return catalogVerificationError(
				"catalog declares a socket-proxy permission outside the allow-list",
				"declare only recognized docker-socket-proxy permissions",
				fmt.Errorf(
					"app %q socket proxy declares permission %q outside the allow-list",
					app.AppID,
					perm,
				),
			)
		}
	}

	networkInternal, networkFound := false, false
	for _, network := range app.Networks {
		if network.Name == proxy.Network {
			networkFound = true
			networkInternal = network.Internal
			break
		}
	}
	if !networkFound {
		return catalogVerificationError(
			"catalog socket proxy references an undeclared network",
			"point socket_proxy.network at a declared internal network",
			fmt.Errorf(
				"app %q socket proxy references network %q absent from the networks declaration",
				app.AppID,
				proxy.Network,
			),
		)
	}
	if !networkInternal {
		return catalogVerificationError(
			"catalog socket proxy network is not internal",
			"mark the socket-proxy network internal:true so the proxy is never reachable off-host",
			fmt.Errorf(
				"app %q socket proxy network %q is not internal",
				app.AppID,
				proxy.Network,
			),
		)
	}

	for _, pin := range app.ImagePins {
		if pin.Service == proxy.Service {
			return nil
		}
	}
	return catalogVerificationError(
		"catalog socket proxy service is not image-pinned",
		"add an image_pins entry for the socket-proxy service",
		fmt.Errorf(
			"app %q socket proxy service %q has no image pin",
			app.AppID,
			proxy.Service,
		),
	)
}

// verifyRenderedNoDirectSocketMount enforces check (B): no rendered service
// binds the Docker socket directly except the declared, enabled proxy sidecar.
// The exemption requires a non-empty proxy service name and an explicit flag, so
// it never collides with the zero value: absent a real proxy, every socket bind
// — including one on an empty-named service — is refused (fail closed). Refusals
// reference rendered compose content, so they are redacted.
func verifyRenderedNoDirectSocketMount(
	redactor security.Redactor,
	app catalog.App,
	projection composeSocketProjection,
) error {
	proxyService, hasProxyExemption := "", false
	if app.SocketProxy != nil && app.SocketProxy.Enabled && app.SocketProxy.Service != "" {
		proxyService, hasProxyExemption = app.SocketProxy.Service, true
	}
	for _, name := range sortedSocketServiceNames(projection) {
		if hasProxyExemption && name == proxyService {
			continue
		}
		for _, volume := range projection.Services[name].Volumes {
			if isDockerSocketSource(volume.source) {
				return redactedVerificationError(
					redactor,
					"rendered compose binds the Docker socket directly into a container",
					"route Docker API access through a declared docker-socket-proxy sidecar; never bind docker.sock directly",
					fmt.Errorf(
						"app %q service %q binds the Docker socket directly",
						app.AppID,
						name,
					),
				)
			}
		}
	}
	return nil
}

// verifyRenderedProxyNetworkInternal enforces check (C): the declared, enabled
// docker-socket-proxy sidecar may attach only to internal networks the catalog
// declares. It runs solely when a real enabled proxy is declared (a non-nil
// SocketProxy with Enabled and a non-empty Service), the same gating as the
// check-(B) exemption, so it never acts on the zero value; absent that it is a
// no-op, which keeps it silent for the curated apps (none declares socket_proxy).
//
// Network-naming convention: wdm templates declare top-level networks as
// external:true, and wdm pre-creates each one with `docker network create`
// (passing --internal when [catalog.Network.Internal] is set) under the exact
// declared name. Because the networks are external, a service's networks list
// references the compose-local key with no project prefix, so each rendered
// network name equals a catalog [catalog.Network.Name]. Check (C) maps the
// proxy's rendered attachments to app.Networks by that name.
//
// It refuses if the proxy is attached to any network that is non-internal or
// absent from the catalog, and it fails closed on absence: a proxy service with
// no networks block joins the project's non-internal default network and would
// be reachable, and a proxy service entirely absent from the rendered services
// is equally a refusal. Refusals derive from the rendered compose, so they route
// through [redactedVerificationError]; any YAML that cannot be classified fails
// closed the same way. All failures map to [types.ErrCodeVerificationFailed].
func verifyRenderedProxyNetworkInternal(
	redactor security.Redactor,
	app catalog.App,
	composeBytes []byte,
) error {
	if app.SocketProxy == nil || !app.SocketProxy.Enabled || app.SocketProxy.Service == "" {
		return nil
	}
	proxyService := app.SocketProxy.Service

	var projection composeIPAMProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for socket-proxy network verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	service, ok := projection.Services[proxyService]
	if !ok {
		return redactedVerificationError(
			redactor,
			"socket-proxy service is absent from the rendered compose",
			"declare the socket-proxy service in the compose template",
			fmt.Errorf(
				"app %q socket proxy service %q is absent from the rendered compose",
				app.AppID,
				proxyService,
			),
		)
	}

	attached := service.Networks.names()
	if len(attached) == 0 {
		return redactedVerificationError(
			redactor,
			"socket-proxy service attaches to no network in the rendered compose",
			"attach the socket-proxy service to the declared internal network so it never joins the default network",
			fmt.Errorf(
				"app %q socket proxy service %q declares no networks and would join the non-internal default network",
				app.AppID,
				proxyService,
			),
		)
	}

	internalNetworks := internalNetworkNames(app)
	for _, network := range attached {
		if _, ok := internalNetworks[network]; !ok {
			return redactedVerificationError(
				redactor,
				"socket-proxy service attaches to a network that is not a declared internal network",
				"attach the socket-proxy service only to catalog networks marked internal:true",
				fmt.Errorf(
					"app %q socket proxy service %q attaches to network %q that is not a catalog internal network",
					app.AppID,
					proxyService,
					network,
				),
			)
		}
	}
	return nil
}

// internalNetworkNames returns the set of catalog network names marked
// internal:true, the allow-list check (C) admits the socket-proxy sidecar to.
func internalNetworkNames(app catalog.App) map[string]struct{} {
	names := make(map[string]struct{}, len(app.Networks))
	for _, network := range app.Networks {
		if network.Internal {
			names[network.Name] = struct{}{}
		}
	}
	return names
}

// hostModulePath is the host kernel-module tree a service needing a host-loaded
// kernel module mounts read-only (PRD §9, §12.2). It pairs with a host-side
// modprobe prerequisite and replaces the excluded SYS_MODULE capability; the
// mount is the sole shape catalog service_hardening host_module_mount permits.
const hostModulePath = "/lib/modules"

// composeModuleProjection is the minimal slice of a rendered docker-compose.yml
// needed to find host /lib/modules bind mounts. Only services[].volumes is
// decoded; yaml.v3 ignores every other key.
type composeModuleProjection struct {
	Services map[string]composeModuleService `yaml:"services"`
}

type composeModuleService struct {
	Volumes []composeModuleVolume `yaml:"volumes"`
}

// composeModuleVolume captures the source, target, and read-only posture of one
// Compose volume entry, reusing [composeVolume]'s normalization of both the
// short "source:target[:mode]" string form and the long {source, target,
// read_only} mapping form. It fails closed on any other node kind so an
// unclassifiable volume is refused rather than silently passed. read_only is
// true when the short form carries the ":ro" mode or the long form sets
// read_only:true.
type composeModuleVolume struct {
	source   string
	target   string
	readOnly bool
}

func (v *composeModuleVolume) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var raw string
		if err := node.Decode(&raw); err != nil {
			return err
		}
		source, rest, hasRest := strings.Cut(raw, ":")
		v.source = source
		if hasRest {
			target, mode, hasMode := strings.Cut(rest, ":")
			v.target = target
			v.readOnly = hasMode && shortVolumeModeIsReadOnly(mode)
		}
		return nil
	case yaml.MappingNode:
		var mapping struct {
			Source   string `yaml:"source"`
			Target   string `yaml:"target"`
			ReadOnly bool   `yaml:"read_only"`
		}
		if err := node.Decode(&mapping); err != nil {
			return err
		}
		v.source = mapping.Source
		v.target = mapping.Target
		v.readOnly = mapping.ReadOnly
		return nil
	default:
		return fmt.Errorf("unexpected compose volume node kind %d", node.Kind)
	}
}

// shortVolumeModeIsReadOnly reports whether a Compose short-form mode field
// (the segment after the second ':') marks the bind read-only. Compose accepts
// a comma-separated mode list (e.g. "ro,z"), so any "ro" element qualifies.
func shortVolumeModeIsReadOnly(mode string) bool {
	for _, part := range strings.Split(mode, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "ro") {
			return true
		}
	}
	return false
}

// isHostModuleSource reports whether a Compose volume host-side source binds the
// host kernel-module tree. It matches the cleaned path so /lib/modules and a
// trailing-slash variant both match, while a named volume (libmodules) or an
// unrelated path (/lib/modules.bak) does not.
func isHostModuleSource(source string) bool {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return false
	}
	return path.Clean(trimmed) == hostModulePath
}

func sortedModuleServiceNames(projection composeModuleProjection) []string {
	names := make([]string, 0, len(projection.Services))
	for name := range projection.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// verifyHostModuleMountMatchCatalog enforces the PRD §9/§12.2 host /lib/modules
// mount policy against the rendered compose, mirroring the container-privilege
// scan ([verifyContainerPrivilegeMatchCatalog]). SYS_MODULE is excluded from the
// allow-list, so a kernel-module need is met by a host modprobe prerequisite
// plus a read-only /lib/modules mount declared via service_hardening
// host_module_mount. It runs three checks and fails closed on any YAML it cannot
// classify:
//
//   - (UNIVERSAL bound) no rendered service may bind the host /lib/modules path
//     unless its catalog ServiceHardening declares host_module_mount:true — a
//     /lib/modules mount on a non-declaring service is unbacked by the signed
//     catalog and is refused;
//   - (PARITY, presence) every service the catalog declares host_module_mount
//     for MUST render a /lib/modules host mount; declared-but-absent is refused;
//     and
//   - (PARITY, shape) a declaring service's mount must bind host /lib/modules to
//     container /lib/modules read-only; a read-write mount, a different container
//     target, or a missing target is refused.
//
// Both render paths ([Engine.renderInstall] and rewriteUpdateStack) call it
// after [verifyContainerPrivilegeMatchCatalog] and [verifySocketPolicyMatchCatalog].
// The catalog declares only a boolean flag (no metadata to validate), so every
// refusal derives from the rendered compose; refusals name the service from
// catalog metadata (not a secret) but route through [redactedVerificationError]
// for parity with the F4/F5 siblings, as does any parse failure. All failures
// map to [types.ErrCodeVerificationFailed].
func verifyHostModuleMountMatchCatalog(
	redactor security.Redactor,
	app catalog.App,
	composeBytes []byte,
) error {
	var projection composeModuleProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for host-module-mount verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	declaredServices := declaredModuleMountServices(app)
	if err := verifyRenderedModuleMountBounds(redactor, app, projection, declaredServices); err != nil {
		return err
	}
	return verifyRenderedModuleMountParity(redactor, app, projection, declaredServices)
}

// declaredModuleMountServices is the set of Compose service names the catalog
// declares host_module_mount:true for. A service outside this set must render no
// /lib/modules mount; a service inside it is governed by the parity check.
func declaredModuleMountServices(app catalog.App) map[string]struct{} {
	declared := make(map[string]struct{})
	for _, hardening := range app.ServiceHardening {
		if hardening.HostModuleMount {
			declared[hardening.Service] = struct{}{}
		}
	}
	return declared
}

// verifyRenderedModuleMountBounds enforces the universal bound: no rendered
// service may bind host /lib/modules unless the catalog declares
// host_module_mount:true for it. declaredServices is the set of service names
// with that declaration; a declaring service's mount shape is governed by the
// parity check instead. Refusals reference rendered compose content, so they are
// redacted.
func verifyRenderedModuleMountBounds(
	redactor security.Redactor,
	app catalog.App,
	projection composeModuleProjection,
	declaredServices map[string]struct{},
) error {
	for _, name := range sortedModuleServiceNames(projection) {
		if _, declared := declaredServices[name]; declared {
			continue
		}
		for _, volume := range projection.Services[name].Volumes {
			if isHostModuleSource(volume.source) {
				return redactedVerificationError(
					redactor,
					"rendered compose mounts the host module tree into an undeclared service",
					"declare host_module_mount in catalog service_hardening or remove the /lib/modules mount from the compose template",
					fmt.Errorf(
						"app %q service %q binds host /lib/modules but the catalog declares no host_module_mount for it",
						app.AppID,
						name,
					),
				)
			}
		}
	}
	return nil
}

// verifyRenderedModuleMountParity enforces that every service the catalog
// declares host_module_mount:true for renders exactly one read-only host
// /lib/modules → /lib/modules mount. A declared-but-absent mount, a read-write
// mount, or a mount to a different container target is refused. Refusals
// reference rendered compose content, so they are redacted.
func verifyRenderedModuleMountParity(
	redactor security.Redactor,
	app catalog.App,
	projection composeModuleProjection,
	declaredServices map[string]struct{},
) error {
	for _, name := range sortedSetValues(declaredServices) {
		service, ok := projection.Services[name]
		if !ok {
			return redactedVerificationError(
				redactor,
				"catalog declares a host module mount for a service absent from the rendered compose",
				"align the catalog service_hardening service names with the compose template",
				fmt.Errorf(
					"app %q declares host_module_mount for service %q but the rendered compose declares no such service",
					app.AppID,
					name,
				),
			)
		}

		var moduleMount *composeModuleVolume
		for i := range service.Volumes {
			if isHostModuleSource(service.Volumes[i].source) {
				moduleMount = &service.Volumes[i]
				break
			}
		}
		if moduleMount == nil {
			return redactedVerificationError(
				redactor,
				"catalog declares a host module mount the rendered compose does not bind",
				"add a read-only /lib/modules:/lib/modules mount to the declaring service or drop the catalog host_module_mount declaration",
				fmt.Errorf(
					"app %q service %q declares host_module_mount but the rendered compose binds no /lib/modules mount",
					app.AppID,
					name,
				),
			)
		}
		if path.Clean(strings.TrimSpace(moduleMount.target)) != hostModulePath {
			return redactedVerificationError(
				redactor,
				"rendered compose host module mount targets the wrong container path",
				"mount host /lib/modules at container /lib/modules read-only",
				fmt.Errorf(
					"app %q service %q binds host /lib/modules to container target %q, not %q",
					app.AppID,
					name,
					moduleMount.target,
					hostModulePath,
				),
			)
		}
		if !moduleMount.readOnly {
			return redactedVerificationError(
				redactor,
				"rendered compose host module mount is not read-only",
				"mount host /lib/modules read-only (append :ro or set read_only:true)",
				fmt.Errorf(
					"app %q service %q binds host /lib/modules read-write; the mount must be read-only",
					app.AppID,
					name,
				),
			)
		}
	}
	return nil
}

// composeIPAMProjection is the minimal slice of a rendered docker-compose.yml
// needed to read each service's per-network static IPv4 attachment. Only
// services[].networks is decoded; yaml.v3 ignores every other key.
type composeIPAMProjection struct {
	Services map[string]composeIPAMService `yaml:"services"`
}

type composeIPAMService struct {
	Networks composeServiceNetworks `yaml:"networks"`
}

// composeServiceNetworks normalizes Compose's two service-networks forms — a
// sequence of bare network names (no static address) and a mapping of network
// name to attachment options ({ipv4_address: ...}) — into one network→address
// shape. A static IPv4 is expressible only via the mapping form, so the sequence
// form records every attached network with an empty address. Any other node
// kind fails closed.
type composeServiceNetworks struct {
	ipv4ByNetwork map[string]string
}

func (n *composeServiceNetworks) UnmarshalYAML(node *yaml.Node) error {
	n.ipv4ByNetwork = map[string]string{}
	switch node.Kind {
	case yaml.SequenceNode:
		var names []string
		if err := node.Decode(&names); err != nil {
			return err
		}
		for _, name := range names {
			n.ipv4ByNetwork[name] = ""
		}
		return nil
	case yaml.MappingNode:
		var mapping map[string]composeNetworkAttachment
		if err := node.Decode(&mapping); err != nil {
			return err
		}
		for name, attachment := range mapping {
			n.ipv4ByNetwork[name] = attachment.IPv4Address
		}
		return nil
	default:
		return fmt.Errorf("unexpected compose service networks node kind %d", node.Kind)
	}
}

// names returns the network names the service attaches to, sorted for
// deterministic iteration. The set spans both the sequence and mapping forms
// normalized by UnmarshalYAML; an empty slice means the service declares no
// networks block. It is a freshly built slice, so callers cannot mutate the
// projection through it.
func (n *composeServiceNetworks) names() []string {
	return sortedStringKeys(n.ipv4ByNetwork)
}

// composeNetworkAttachment carries one service-to-network attachment's static
// address. A null mapping value (e.g. "front:" with no options) decodes to the
// zero value, which is the no-static-address case.
type composeNetworkAttachment struct {
	IPv4Address string `yaml:"ipv4_address"`
}

// verifyNetworkIPAMMatchCatalog enforces the PRD §9 static-addressing policy
// against the rendered compose, mirroring the container-privilege scan
// ([verifyContainerPrivilegeMatchCatalog]). Templates author ipv4_address
// literally; the catalog declares the IPAM; internal/core verifies parity and
// bounds. It runs three checks and fails closed on any YAML it cannot classify:
//
//   - (CATALOG validation) for each network with IPAM the subnet must be a valid
//     IPv4 CIDR, a set gateway must be an IPv4 within the subnet, and each
//     declared address must be an IPv4 within the subnet naming a real compose
//     service. These refusals read only catalog metadata, so they are
//     non-redacting [catalogVerificationError]s;
//   - (PARITY) every catalog-declared per-service address must equal the address
//     the rendered compose pins for that service on that network; a declared
//     address missing or different in the rendered compose is refused; and
//   - (UNIVERSAL bound) no rendered service may pin an ipv4_address the catalog
//     IPAM does not declare — a tampered template cannot fix an unintended static
//     IP.
//
// Both render paths ([Engine.renderInstall] and rewriteUpdateStack) call it
// after [verifyHostModuleMountMatchCatalog]. Every refusal derived from the
// rendered compose, and any parse failure, is routed through
// [redactedVerificationError]; catalog/template metadata carries no secrets but
// is redacted defensively for parity with the sibling render errors. All
// failures map to [types.ErrCodeVerificationFailed].
func verifyNetworkIPAMMatchCatalog(
	redactor security.Redactor,
	app catalog.App,
	composeBytes []byte,
) error {
	declared, err := validateCatalogIPAMDeclarations(app)
	if err != nil {
		return err
	}

	var projection composeIPAMProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for network IPAM verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	if err := verifyRenderedIPAMParity(redactor, app, projection, declared); err != nil {
		return err
	}
	return verifyRenderedIPAMBounds(redactor, app, projection, declared)
}

// ipamAddressKey identifies one declared static assignment by network and
// service, the key the parity and universal-bound checks compare on.
type ipamAddressKey struct {
	network string
	service string
}

// validateCatalogIPAMDeclarations enforces the catalog-validation check and
// returns the declared per-(network,service) static addresses, normalized to
// their canonical netip string form so the rendered comparison is exact. It
// reads only catalog metadata, so refusals are non-redacting.
func validateCatalogIPAMDeclarations(app catalog.App) (map[ipamAddressKey]string, error) {
	declared := map[ipamAddressKey]string{}
	for _, network := range app.Networks {
		if network.IPAM == nil {
			continue
		}
		prefix, err := netip.ParsePrefix(network.IPAM.Subnet)
		if err != nil || !prefix.Addr().Is4() {
			return nil, catalogVerificationError(
				"catalog declares an invalid IPAM subnet",
				"declare an IPv4 CIDR such as 10.0.0.0/24",
				fmt.Errorf(
					"app %q network %q declares subnet %q that is not a valid IPv4 CIDR",
					app.AppID,
					network.Name,
					network.IPAM.Subnet,
				),
			)
		}
		canonicalSubnet := prefix.Masked()

		if network.IPAM.Gateway != "" {
			gateway, err := netip.ParseAddr(network.IPAM.Gateway)
			if err != nil || !gateway.Is4() || !canonicalSubnet.Contains(gateway) {
				return nil, catalogVerificationError(
					"catalog declares an IPAM gateway outside the subnet",
					"declare a gateway that is a valid IPv4 within the subnet",
					fmt.Errorf(
						"app %q network %q declares gateway %q outside subnet %q",
						app.AppID,
						network.Name,
						network.IPAM.Gateway,
						network.IPAM.Subnet,
					),
				)
			}
		}

		for _, address := range network.IPAM.Addresses {
			addr, err := netip.ParseAddr(address.IPv4Address)
			if err != nil || !addr.Is4() || !canonicalSubnet.Contains(addr) {
				return nil, catalogVerificationError(
					"catalog declares an IPAM address outside the subnet",
					"declare each static address as a valid IPv4 within the subnet",
					fmt.Errorf(
						"app %q network %q declares address %q outside subnet %q",
						app.AppID,
						network.Name,
						address.IPv4Address,
						network.IPAM.Subnet,
					),
				)
			}
			if !serviceDeclaredInCatalog(app, address.Service) {
				return nil, catalogVerificationError(
					"catalog declares an IPAM address for an unknown service",
					"point each IPAM address at a service the catalog declares",
					fmt.Errorf(
						"app %q network %q declares address for service %q not present in the catalog",
						app.AppID,
						network.Name,
						address.Service,
					),
				)
			}
			declared[ipamAddressKey{network: network.Name, service: address.Service}] = addr.String()
		}
	}
	return declared, nil
}

// serviceDeclaredInCatalog reports whether the catalog names the service through
// an image pin or a port declaration — the two surfaces that enumerate the real
// compose services. An IPAM address pointing elsewhere is a catalog error.
func serviceDeclaredInCatalog(app catalog.App, service string) bool {
	for _, pin := range app.ImagePins {
		if pin.Service == service {
			return true
		}
	}
	for _, port := range app.Ports {
		if port.Service == service {
			return true
		}
	}
	return false
}

// verifyRenderedIPAMParity enforces the parity check: every declared static
// address must equal the address the rendered compose pins for that service on
// that network. Refusals reference rendered compose content, so they are
// redacted.
func verifyRenderedIPAMParity(
	redactor security.Redactor,
	app catalog.App,
	projection composeIPAMProjection,
	declared map[ipamAddressKey]string,
) error {
	for _, key := range sortedIPAMKeys(declared) {
		service, ok := projection.Services[key.service]
		if !ok {
			return redactedVerificationError(
				redactor,
				"catalog declares an IPAM address for a service absent from the rendered compose",
				"align the catalog IPAM address service names with the compose template",
				fmt.Errorf(
					"app %q declares an IPAM address for service %q but the rendered compose declares no such service",
					app.AppID,
					key.service,
				),
			)
		}
		rendered, attached := service.Networks.ipv4ByNetwork[key.network]
		if !attached || rendered != declared[key] {
			return redactedVerificationError(
				redactor,
				"rendered compose static IP does not match the catalog IPAM declaration",
				"align the compose template ipv4_address with the catalog IPAM address",
				fmt.Errorf(
					"app %q service %q network %q: catalog declares static IP %q but the rendered compose pins %q",
					app.AppID,
					key.service,
					key.network,
					declared[key],
					rendered,
				),
			)
		}
	}
	return nil
}

// verifyRenderedIPAMBounds enforces the universal bound: no rendered service may
// pin an ipv4_address the catalog IPAM does not declare. Refusals reference
// rendered compose content, so they are redacted.
func verifyRenderedIPAMBounds(
	redactor security.Redactor,
	app catalog.App,
	projection composeIPAMProjection,
	declared map[ipamAddressKey]string,
) error {
	for _, serviceName := range sortedIPAMServiceNames(projection) {
		networks := projection.Services[serviceName].Networks.ipv4ByNetwork
		for _, networkName := range sortedStringKeys(networks) {
			if networks[networkName] == "" {
				continue
			}
			if _, ok := declared[ipamAddressKey{network: networkName, service: serviceName}]; !ok {
				return redactedVerificationError(
					redactor,
					"rendered compose pins a static IP the catalog IPAM does not declare",
					"a static ipv4_address must be declared in the catalog network ipam addresses",
					fmt.Errorf(
						"app %q service %q network %q pins a static IP with no catalog IPAM declaration",
						app.AppID,
						serviceName,
						networkName,
					),
				)
			}
		}
	}
	return nil
}

func sortedIPAMKeys(declared map[ipamAddressKey]string) []ipamAddressKey {
	keys := make([]ipamAddressKey, 0, len(declared))
	for key := range declared {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].network != keys[j].network {
			return keys[i].network < keys[j].network
		}
		return keys[i].service < keys[j].service
	})
	return keys
}

func sortedIPAMServiceNames(projection composeIPAMProjection) []string {
	names := make([]string, 0, len(projection.Services))
	for name := range projection.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type redactedCause struct {
	message string
	unwrap  error
}

func (e redactedCause) Error() string {
	return e.message
}

func (e redactedCause) Unwrap() error {
	return e.unwrap
}

func redactedVerificationError(
	redactor security.Redactor,
	message string,
	hint string,
	cause error,
) error {
	return types.WrapError(
		types.ErrCodeVerificationFailed,
		message,
		hint,
		newRedactedCause(redactor, cause),
	)
}

func newRedactedCause(redactor security.Redactor, cause error) redactedCause {
	message := fmt.Sprint(cause)
	if redactor != nil {
		message = redactor.Redact(message)
	}
	return redactedCause{
		message: message,
		unwrap:  redactedUnwrap(cause),
	}
}

func redactedUnwrap(cause error) error {
	for _, sentinel := range []error{
		render.ErrEnvTemplateParse,
		render.ErrEnvTemplateExecute,
		render.ErrComposeTemplateParse,
		render.ErrComposeTemplateExecute,
		render.ErrComposeYAMLParse,
		render.ErrComposeYAMLEncode,
		render.ErrComposeServicesMissing,
		render.ErrServiceMissingLabel,
		render.ErrAdditionalFileMountMissing,
		render.ErrAdditionalFileTemplateParse,
		render.ErrAdditionalFileTemplateExecute,
	} {
		if errors.Is(cause, sentinel) {
			return sentinel
		}
	}
	return nil
}

// completedServiceNamePattern is the conservative Compose service-name
// shape a completed_services entry must match: a leading alphanumeric
// followed by alphanumerics, underscores, dots, or hyphens. It rejects
// empty, whitespace, and path-like names before the membership checks
// run, so a tampered catalog cannot smuggle an odd name past the
// cross-reference.
var completedServiceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// verifyCompletedServicesMatchCatalog cross-checks every catalog
// completed_services entry against the install's authoritative service
// sets before the manifest is written. A name earns the
// completed-by-design exemption from container_exited only when it (1)
// matches the conservative Compose service-name shape, (2) is pinned in
// image_pins, and (3) renders as a real service in the compose template
// (serviceLabels keys, populated before the verify chain runs). Any
// other name fails the install closed via the shared
// [catalogVerificationError] so a drifted or tampered catalog can never
// mark an unknown — or unexpected-exit — service "completed". An empty
// completed_services list is valid and verifies as a no-op.
func verifyCompletedServicesMatchCatalog(app catalog.App, serviceLabels map[string]map[string]string) error {
	if len(app.CompletedServices) == 0 {
		return nil
	}

	pinned := make(map[string]struct{}, len(app.ImagePins))
	for _, pin := range app.ImagePins {
		if pin.Service == "" {
			continue
		}
		pinned[pin.Service] = struct{}{}
	}

	for _, service := range app.CompletedServices {
		if !completedServiceNamePattern.MatchString(service) {
			return catalogVerificationError(
				"catalog completed_services names an invalid compose service",
				"list only plain compose service names in completed_services",
				fmt.Errorf(
					"app %q completed service %q is not a valid compose service name",
					app.AppID,
					service,
				),
			)
		}
		if _, ok := pinned[service]; !ok {
			return catalogVerificationError(
				"catalog completed_services names a service absent from image_pins",
				"every completed service must also be pinned in image_pins",
				fmt.Errorf(
					"app %q lists completed service %q with no matching image_pins entry",
					app.AppID,
					service,
				),
			)
		}
		if _, ok := serviceLabels[service]; !ok {
			return catalogVerificationError(
				"catalog completed_services names a service absent from the rendered compose",
				"align completed_services with the compose template service names",
				fmt.Errorf(
					"app %q lists completed service %q with no matching rendered compose service",
					app.AppID,
					service,
				),
			)
		}
	}
	return nil
}
