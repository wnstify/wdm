package catalog

import "time"

// Catalog is the typed form of a catalog.yaml manifest validated by
// [LoadCatalog] / [LoadCatalogBytes] against catalog/schema.json. Field
// tags carry both yaml:"..." (the on-disk format) and json:"..."
// (forward-compat for callers that surface catalog content through the
// JSON envelope in pkg/types).
// The struct mirrors PRD §22.
// Adding or removing a field here changes the catalog contract and MUST
// stay in lockstep with catalog/schema.json, the validation gate. An
// additive optional field may land in the schema first and gain its
// decode here later, since validation runs before this struct is
// populated.
type Catalog struct {
	// SchemaVersion is the catalog schema version. The schema accepts
	// 1 or 2 and both validate: version 2 carries the additive v2
	// declaration fields (public-port intent, service hardening,
	// socket proxy, config generation, network IPAM), each optional, so
	// a version-1 manifest still loads (PRD §22, §30).
	SchemaVersion int `yaml:"schema_version" json:"schema_version"`

	// Channel is the catalog channel this manifest belongs to.
	// v1 ships "stable" only; "verified" is reserved for a future
	// premium channel (PRD §22). Enforced by the schema's enum.
	Channel string `yaml:"channel" json:"channel"`

	// GeneratedAt is the RFC 3339 timestamp this manifest was
	// generated at. The schema declares format:"date-time"; the
	// loader's step 1 (YAML→intermediate→JSON-normalize→map)
	// stringifies the value via json.Marshal so the validator sees
	// the JSON-canonical form, while step 3 re-decodes the original
	// bytes into this [time.Time] field via yaml.v3's
	// struct-tag-guided parser.
	GeneratedAt time.Time `yaml:"generated_at" json:"generated_at"`

	// Apps lists the curated app templates available in this
	// channel. PRD §15 documents v1 eligibility rules.
	Apps []App `yaml:"apps" json:"apps"`
}

// App is a single entry in [Catalog.Apps]. Every exported field maps to
// a required property in catalog/schema.json's $defs/app definition
// (additionalProperties:false at every nesting level, so missing or
// extra fields fail validation in the loader's step 2).
type App struct {
	// AppID is the stable identifier used as the default stack
	// subdirectory name (~/docker/<app_id>) and Compose project
	// suffix (wdm-<app_id>). PRD §9, §17. Pattern enforced by
	// the schema: lowercase ASCII + digits + hyphen, starting
	// with a letter, length 1-63.
	AppID string `yaml:"app_id" json:"app_id"`

	// Name is the human-readable app name shown in the catalog
	// browser and on the install finish screen.
	Name string `yaml:"name" json:"name"`

	// Summary is the one-line description rendered in list views.
	Summary string `yaml:"summary" json:"summary"`

	// Description is the long-form description rendered on the
	// app detail screen. May contain Markdown (renderer is a
	Description string `yaml:"description" json:"description"`

	// TemplateName is the human-readable template label this
	// app's Compose /.env files are derived from. Stored
	// verbatim in.wdm.lock at install time.
	TemplateName string `yaml:"template_name" json:"template_name"`

	// TemplateVersion is the template's version at the time the
	// catalog manifest was generated. Compared against newer
	// catalog versions during update checks (PRD §20).
	TemplateVersion string `yaml:"template_version" json:"template_version"`

	// ComposeTemplate references the Compose template the renderer
	// uses. Template paths are resolved by internal/core before render.
	ComposeTemplate string `yaml:"compose_template" json:"compose_template"`

	// EnvTemplate references the .env template. It uses the same
	// path-resolution semantics as ComposeTemplate.
	EnvTemplate string `yaml:"env_template" json:"env_template"`

	// Placeholders enumerates the templated values the renderer
	// substitutes into ComposeTemplate / EnvTemplate.
	Placeholders []Placeholder `yaml:"placeholders" json:"placeholders"`

	// SupportedVersions declares the Docker engine + Compose
	// plugin constraints the app's template assumes. Validated
	// at install time against the host's actual versions
	// (PRD §2).
	SupportedVersions SupportedVersions `yaml:"supported_versions" json:"supported_versions"`

	// Ports lists the local ports the rendered stack binds
	// (PRD §9). Host-side ports may be overridden during install.
	Ports []Port `yaml:"ports" json:"ports"`

	// ImagePins lists the per-service image references the
	// catalog pins for this app. Tag is mandatory in the catalog
	// shape; the resolved digest is captured into.wdm.lock
	// at install/update time (PRD §9, §22).
	ImagePins []ImagePin `yaml:"image_pins" json:"image_pins"`

	// Networks lists the Docker networks the rendered stack
	// expects to exist before `docker compose up -d`.
	// `internal/docker.EnsureNetwork` creates each one via
	// `docker network create [--internal] <name>` before deploy
	// (idempotent skip-if-exists) and leaves them in place on
	// safe-remove. Optional — apps with no external networks omit
	Networks []Network `yaml:"networks,omitempty" json:"networks,omitempty"`

	// AdditionalFiles lists sidecar template artifacts the
	// renderer writes to the stack directory alongside
	// `docker-compose.yml` and `.env` (DB init scripts,
	// web-server configs, TOML/YAML/JSON configs, and any
	// nested-path file). Each entry's optional `Mount` is a
	// documentation/validation field: `internal/render` verifies
	// the declared mount is already present in the Compose YAML
	// and refuses to proceed otherwise — render NEVER injects
	// mounts. Optional — apps without sidecars omit it.
	AdditionalFiles []AdditionalFile `yaml:"additional_files,omitempty" json:"additional_files,omitempty"`

	// Resources lists per-service resource sizing bands used by
	// install planning for host-capacity refusal, default
	// selection, and user-override rejection. `internal/system`
	// probes host CPU + total memory at install time, selects
	// `recommended` when it fits the host budget, falls back to
	// `min` with a warning if not, refuses install if `min`
	// doesn't fit either, and rejects out-of-band user overrides
	// rather than clamping them. Optional —
	// apps without explicit sizing fall back to template-declared
	// `deploy.resources.limits` at render time, but lose wdm's
	// ability to refuse install or reject overrides safely.
	Resources []ResourceProfile `yaml:"resources,omitempty" json:"resources,omitempty"`

	// ServiceHardening carries per-service container-privilege
	// declarations validated against the PRD §12.2 closed allow-list.
	// Absent means every service keeps the cap_drop:ALL baseline and
	// runs unprivileged — no added capabilities, sysctls, devices,
	// ulimits, or host module mount. Schema version 2.
	ServiceHardening []ServiceHardening `yaml:"service_hardening,omitempty" json:"service_hardening,omitempty"`

	// SocketProxy is the optional docker-socket-proxy declaration
	// (PRD §12.1). Docker API access reaches the app only through this
	// sidecar on an --internal network; wdm refuses any direct
	// /var/run/docker.sock bind. The pointer distinguishes an absent
	// block (nil → no Docker socket access) from a zero-valued one.
	// Schema version 2.
	SocketProxy *SocketProxy `yaml:"socket_proxy,omitempty" json:"socket_proxy,omitempty"`

	// ConfigGeneration lists the artifacts wdm renders from the
	// placeholder map at install (PRD §17). Distinct from
	// AdditionalFiles: those are static sidecars wdm copies, while
	// these are templates wdm renders itself — it never invokes an
	// upstream generate-config.sh. Schema version 2.
	ConfigGeneration []ConfigGenerationArtifact `yaml:"config_generation,omitempty" json:"config_generation,omitempty"`

	// LocalTargetURLTemplate is the install-finish local URL shown
	// after deployment (PRD §16, §17 step 14). It may carry Go
	// template placeholder references resolved against the install's
	// placeholder map (e.g. "http://127.0.0.1:{{.PORT_3008 }}/").
	// Optional — when absent, install guidance falls back to
	// "http://127.0.0.1:<first ports host port>" per
	LocalTargetURLTemplate string `yaml:"local_target_url_template,omitempty" json:"local_target_url_template,omitempty"`

	// PangolinGuidance is the structured reverse-proxy guidance
	// shown on the install finish screen (PRD §16).
	PangolinGuidance PangolinGuidance `yaml:"pangolin_guidance" json:"pangolin_guidance"`

	// FirstRunNotes are optional first-run checklist lines surfaced
	// on the install finish screen alongside the Pangolin guidance
	// README prose is never parsed; the
	// catalog carries the structured lines directly.
	FirstRunNotes []string `yaml:"first_run_notes,omitempty" json:"first_run_notes,omitempty"`

	// CompletedServices lists the Compose service names that complete
	// by design once they run to success — one-shot init containers
	// that exit 0 rather than staying up. Status logic treats a service
	// named here as done instead of needs_attention. Optional —
	// apps with no init containers omit it. The schema validates
	// non-empty, unique string items; cross-referencing each name
	// against the app's services is enforced upstream, not here.
	CompletedServices []string `yaml:"completed_services,omitempty" json:"completed_services,omitempty"`

	// RiskClassification carries one or more PRD §20 risk tags
	// surfaced on the install finish screen and gating the
	// database-risk confirmation flow. Bare strings are rejected at
	// schema validation. Allowed values: "safe",
	// "major", "database", "complex".
	RiskClassification []string `yaml:"risk_classification" json:"risk_classification"`
}

// PangolinGuidance is the structured reverse-proxy guidance carried by
// an [App] (PRD §16). All fields are optional at
// the schema level so guidance-light apps can ship an empty object; the
// install path omits empty guidance from results.
type PangolinGuidance struct {
	// TargetURL is the local service URL a reverse proxy should
	// point at (e.g. "http://127.0.0.1:3008").
	TargetURL string `yaml:"target_url,omitempty" json:"target_url,omitempty"`

	// RecommendedSubdomain is the suggested public subdomain label
	// (e.g. "status" for status.<your-domain>).
	RecommendedSubdomain string `yaml:"recommended_subdomain,omitempty" json:"recommended_subdomain,omitempty"`

	// Notes carries optional operator guidance lines (guide links,
	// own-reverse-proxy pointers).
	Notes []string `yaml:"notes,omitempty" json:"notes,omitempty"`
}

// SupportedVersions is the constraint pair declared by an [App]. Each
// value is a free-form constraint string (e.g. ">=20.10"). The host
// version probe enforces the project-wide Docker 20.10+ / Compose v2
// floor via internal/system, not these per-app strings.
type SupportedVersions struct {
	// Docker is the Docker engine version constraint (PRD §2).
	Docker string `yaml:"docker" json:"docker"`

	// Compose is the Compose plugin version constraint. PRD §2
	// requires Compose V2 across the board.
	Compose string `yaml:"compose" json:"compose"`
}

// Placeholder is a single templated value inside an [App]'s Compose and
// .env templates. [Type] is a closed enum; [Encoding] carries the secret
// output format; [Regenerable] is an optional per-secret rotation flag.
type Placeholder struct {
	// Name is the placeholder identifier as it appears in
	// templates (uppercase snake, e.g. "DB_PASSWORD"). Pattern
	// enforced by the schema: ^[A-Z][A-Z0-9_]*$.
	// additionally rejects names overlapping the SQL identifier
	// denylist when [App.AdditionalFiles] declares an init script
	Name string `yaml:"name" json:"name"`

	// Type is the placeholder leaf type. It is narrowed to the closed enum
	// {string, domain, port, secret, timezone, path, bool}; extending it
	// requires an explicit schema bump.
	Type string `yaml:"type" json:"type"`

	// Required indicates whether the installer must supply a
	// value (true) or may accept the Default (false).
	Required bool `yaml:"required" json:"required"`

	// Encoding selects the output form for generated secret values
	// Only allowed when [Type] is "secret";
	// the schema requires it there and rejects it elsewhere. Allowed
	// values mirror internal/security.Encoding: "base64url" for
	// password-like secrets, "hex" for token-like secrets, "base64std"
	// for a standard-base64 encoding of 32 random bytes (for consumers
	// requiring the standard alphabet and an exact 32-byte decoded key),
	// and "argon2id" for a one-way PHC hash whose random plaintext is
	// surfaced to the operator once (must be regenerable:false).
	Encoding string `yaml:"encoding,omitempty" json:"encoding,omitempty"`

	// Default is the optional default value. Its concrete type
	// mirrors the leaf type of [Type]; the schema accepts
	// string/number/boolean/null and internal/render validates the
	// type match at render time. [any] lets unmarshaling preserve
	// whatever YAML scalar the catalog declared.
	Default any `yaml:"default,omitempty" json:"default,omitempty"`

	// Regenerable is the per-secret rotation policy. The
	// schema rejects the field on non-secret placeholders so
	// catalog authors cannot pretend it has any effect there. The
	// pointer is intentional: nil means "use the default" (true,
	// install always regenerates); an explicit false marks the
	// secret as fixed-after-install, and the update path
	// reuses the existing .env value via state.ReadStackEnv
	// Consumers MUST treat nil as true —
	// a non-pointer bool would default to false and silently invert
	// the documented semantics.
	Regenerable *bool `yaml:"regenerable,omitempty" json:"regenerable,omitempty"`
}

// Port is a single port declaration in an [App.Ports] entry.
// Container and Host are bounded to 1..65535 by the schema; the
// validator catches out-of-range values before this struct is
// populated.
type Port struct {
	// Service is the Compose service name the port applies to.
	Service string `yaml:"service" json:"service"`

	// Container is the in-container port the service listens on.
	Container int `yaml:"container" json:"container"`

	// Host is the default host-side port. May be overridden
	// during install to resolve collisions.
	Host int `yaml:"host" json:"host"`

	// Protocol is "tcp" or "udp" (enforced by the schema enum);
	// the schema declares a default of "tcp", which the renderer
	// applies when the field is empty here.
	Protocol string `yaml:"protocol,omitempty" json:"protocol,omitempty"`

	// Public selects the bind interface. Absent or false binds
	// 127.0.0.1; true binds 0.0.0.0 (PRD §11, §11.1). The refusal of
	// a public bind for a management/admin/web-UI port and the
	// accompanying warning + confirmation are enforced in
	// internal/core, not here. Schema version 2.
	Public bool `yaml:"public,omitempty" json:"public,omitempty"`

	// HostRange is the inclusive host-side span "<lo>-<hi>" for apps
	// that publish a contiguous range (e.g. WebRTC media). Paired with
	// ContainerRange; the bounds, lo<=hi, and host/container span match
	// are validated in internal/core. Schema version 2.
	HostRange string `yaml:"host_range,omitempty" json:"host_range,omitempty"`

	// ContainerRange is the container-side span paired with HostRange,
	// subject to the same bounds and span rules. Schema version 2.
	ContainerRange string `yaml:"container_range,omitempty" json:"container_range,omitempty"`
}

// ImagePin is a single service-to-image binding inside an
// [App.ImagePins] entry. The shape differs from
// internal/state.ImagePin: the catalog form requires Tag (the
// reference the maintainer chose at catalog-build time), while
// the state form makes Tag and Digest both optional because
// installers may pin by digest only after resolution.
type ImagePin struct {
	// Service is the Compose service name (e.g. "app", "db").
	Service string `yaml:"service" json:"service"`

	// Image is the image reference without tag or digest
	// (e.g. "vaultwarden/server").
	Image string `yaml:"image" json:"image"`

	// Tag is the image tag (e.g. "1.30.1"). Required by the
	// catalog schema — catalog-time pins always carry a tag, and
	// the digest is resolved at install/update time and stored
	// in.wdm.lock.
	Tag string `yaml:"tag" json:"tag"`

	// Digest is the optional resolved digest (sha256:...). The
	// schema enforces ^sha256:[a-f0-9]{64}$ when present.
	Digest string `yaml:"digest,omitempty" json:"digest,omitempty"`
}

// Network is a single Docker network declaration in an [App.Networks]
// entry. wdm creates each network via
// `docker network create [--internal] <name>` before
// `docker compose up -d` (idempotent skip-if-exists) and leaves it in
// place on safe-remove. Network creation lives in
// `internal/docker.EnsureNetwork` and runs in
type Network struct {
	// Name is the Docker network name passed to `docker network
	// create`. Lowercase ASCII + digits + underscore + hyphen,
	// starting with a letter, length 1-63 (Docker's network-name
	// constraint, enforced by the schema pattern).
	Name string `yaml:"name" json:"name"`

	// Internal, when true, causes wdm to pass `--internal`
	// to `docker network create`, isolating the network from
	// internet egress. Backend DB / cache networks set this to
	// true; user-facing 'front' networks set false.
	Internal bool `yaml:"internal" json:"internal"`

	// IPAM is the optional static IP address management for this
	// network (PRD §9). Nil means Docker's default bridge addressing;
	// a value pins the subnet and optional per-service addresses.
	// Schema version 2.
	IPAM *NetworkIPAM `yaml:"ipam,omitempty" json:"ipam,omitempty"`
}

// NetworkIPAM is the optional static IP address management for a
// [Network] (PRD §9, schema version 2). Absent on the parent means
// Docker's default bridge addressing; a value pins the subnet and any
// per-service addresses. The schema validates the CIDR/IPv4 shapes;
// internal/core validates the octet bounds and subnet membership.
type NetworkIPAM struct {
	// Subnet is the IPv4 CIDR the network uses (e.g. "10.0.0.0/24").
	// internal/core validates the octet bounds.
	Subnet string `yaml:"subnet" json:"subnet"`

	// Gateway is the optional IPv4 gateway within the subnet.
	Gateway string `yaml:"gateway,omitempty" json:"gateway,omitempty"`

	// Addresses pins per-service static IPv4 addresses within the
	// subnet. Optional — absent leaves Docker to assign addresses.
	Addresses []IPAMAddress `yaml:"addresses,omitempty" json:"addresses,omitempty"`
}

// IPAMAddress is a single per-service static IPv4 assignment in a
// [NetworkIPAM.Addresses] entry.
type IPAMAddress struct {
	// Service is the Compose service name receiving the fixed address.
	Service string `yaml:"service" json:"service"`

	// IPv4Address is the static IPv4 address within the subnet.
	IPv4Address string `yaml:"ipv4_address" json:"ipv4_address"`
}

// AdditionalFile is a sidecar template artifact written to the
// stack directory alongside `docker-compose.yml` and `.env`.
// Covers DB init scripts, web-server configs (nginx.conf,
// Caddyfile), TOML/YAML/JSON configs, and any nested-path file.
// flock; the directory snapshot backs them up; the
// sad-path restore re-copies them byte-for-byte.
type AdditionalFile struct {
	// Src is the path to the template-side source file, relative
	// to the app's template directory. May contain nested
	// subdirectories (e.g. "init-scripts/init-garage.sh"). The
	// schema rejects absolute paths and parent-directory
	// traversal ("..").
	Src string `yaml:"src" json:"src"`

	// Dest is the path to the stack-side destination file,
	// relative to the stack directory. May contain nested
	// subdirectories — the writer creates intermediate
	// directories with mode 0o755 (parent-dir fsync covered by
	// the atomic-write helper).
	Dest string `yaml:"dest" json:"dest"`

	// Mode is the POSIX file mode in octal notation with a
	// leading zero (e.g. "0644" for configs, "0755" for
	// executable scripts). Schema pattern: ^0[0-7]{3}$.
	Mode string `yaml:"mode" json:"mode"`

	// Mount is the optional Compose volume-mount declaration the
	// Compose template MUST already carry (e.g.
	// "./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro").
	// `internal/render` walks the parsed Compose YAML and
	// refuses to proceed if the declared mount is absent — render
	// NEVER injects mounts. Omitted means
	// "no mount expected; the file is written for the stack's
	// own consumption" (e.g. a config template the app reads
	// directly from the stack dir).
	Mount string `yaml:"mount,omitempty" json:"mount,omitempty"`
}

// ServiceHardening is a single per-service container-privilege
// declaration in [App.ServiceHardening] (PRD §12.2, schema version 2).
// Every field above the cap_drop:ALL baseline is opt-in and validated
// against wdm's closed allow-list; the renderer applies the baseline
// regardless and layers only the additions declared here.
type ServiceHardening struct {
	// Service is the Compose service name this declaration hardens.
	Service string `yaml:"service" json:"service"`

	// Capabilities lists Linux capabilities to re-add on top of the
	// cap_drop:ALL baseline. Nil means no additions. Pointer so an
	// absent block is distinguishable from an empty one.
	Capabilities *Capabilities `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`

	// Sysctls lists the sysctls the service sets, each name drawn from
	// the PRD §12.2 allow-list (schema enum).
	Sysctls []Sysctl `yaml:"sysctls,omitempty" json:"sysctls,omitempty"`

	// Devices is the device-map list. The PRD §12.2 device allow-list
	// is currently empty, so the schema holds this at zero items; the
	// field is present for completeness and forward compatibility.
	Devices []string `yaml:"devices,omitempty" json:"devices,omitempty"`

	// Privileged reports whether the service runs privileged. Absent
	// means false; true is refused by internal/core absent a recorded
	// PRD amendment.
	Privileged bool `yaml:"privileged,omitempty" json:"privileged,omitempty"`

	// Ulimits carries the per-service ulimits. Nil means image
	// defaults; only nofile is modeled in schema version 2.
	Ulimits *Ulimits `yaml:"ulimits,omitempty" json:"ulimits,omitempty"`

	// HostModuleMount, when true, signals the renderer to expect a
	// /lib/modules:ro host mount for a service needing a host-loaded
	// kernel module (e.g. WireGuard). It pairs with a host-side
	// modprobe prerequisite and replaces the excluded SYS_MODULE
	// capability per PRD §12.2.
	HostModuleMount bool `yaml:"host_module_mount,omitempty" json:"host_module_mount,omitempty"`
}

// Capabilities is the capability-addition set in
// [ServiceHardening.Capabilities]. It carries only the additive set;
// the cap_drop:ALL baseline is applied by the renderer independently.
type Capabilities struct {
	// Add lists the capabilities re-added on top of cap_drop:ALL.
	// Each value is a PRD §12.2 allow-list capability enforced by the
	// schema enum.
	Add []string `yaml:"add" json:"add"`
}

// Sysctl is a single sysctl entry in [ServiceHardening.Sysctls].
type Sysctl struct {
	// Name is the sysctl key, drawn from the PRD §12.2 allow-list
	// (schema enum).
	Name string `yaml:"name" json:"name"`

	// Value is the sysctl value as a string.
	Value string `yaml:"value" json:"value"`
}

// Ulimits is the per-service ulimit set in [ServiceHardening.Ulimits].
// Only nofile is modeled in schema version 2; another ulimit requires
// a schema bump.
type Ulimits struct {
	// Nofile is the optional open-file-descriptor limit
	// (RLIMIT_NOFILE). Nil means the image default.
	Nofile *NofileLimit `yaml:"nofile,omitempty" json:"nofile,omitempty"`
}

// NofileLimit is the open-file-descriptor limit in [Ulimits.Nofile].
// Both bounds are required by the schema.
type NofileLimit struct {
	// Soft is the soft RLIMIT_NOFILE limit.
	Soft int `yaml:"soft" json:"soft"`

	// Hard is the hard RLIMIT_NOFILE limit.
	Hard int `yaml:"hard" json:"hard"`
}

// SocketProxy is the docker-socket-proxy declaration in
// [App.SocketProxy] (PRD §12.1, schema version 2). Docker API access
// reaches the app only through this sidecar on an --internal network;
// wdm refuses any direct /var/run/docker.sock bind.
type SocketProxy struct {
	// Enabled reports whether the socket-proxy sidecar is active.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Service is the Compose service name of the proxy sidecar. Its
	// image is pinned in [App.ImagePins]; the pairing is cross-checked
	// in internal/core.
	Service string `yaml:"service" json:"service"`

	// AllowedAPI is the closed set of Docker API permissions the proxy
	// exposes. Read-scoped flags form the baseline; POST is the
	// write/control switch, declared explicitly per app (PRD §12.1).
	AllowedAPI []string `yaml:"allowed_api" json:"allowed_api"`

	// Network is the name of the network the proxy and its consumer
	// share. It must reference a [App.Networks] entry with Internal
	// true (cross-checked in internal/core) so the proxy is never
	// reachable off-host.
	Network string `yaml:"network" json:"network"`
}

// ConfigGenerationArtifact is a single artifact wdm renders from the
// placeholder map in [App.ConfigGeneration] (PRD §17, schema version 2).
// Distinct from [AdditionalFile]: the source is a template wdm renders,
// not a static file it copies.
type ConfigGenerationArtifact struct {
	// Template is the template-side source path, relative to the app's
	// template directory. wdm renders it from the placeholder map; the
	// schema guards against absolute paths and "..".
	Template string `yaml:"template" json:"template"`

	// Dest is the stack-side destination path, relative to the stack
	// directory; the schema applies the same traversal guard.
	Dest string `yaml:"dest" json:"dest"`

	// Mode is the POSIX file mode in octal notation (e.g. "0640").
	Mode string `yaml:"mode" json:"mode"`

	// Mount is the optional Compose mount the rendered artifact maps
	// to. When set, internal/render verifies the declared mount is
	// already present in the Compose YAML and never injects it.
	Mount string `yaml:"mount,omitempty" json:"mount,omitempty"`
}

// ResourceProfile is a single per-service resource sizing entry in
// [App.Resources]. The bands drive three behaviors:
// install-time refusal when the host cannot satisfy [Memory].Min or
// [CPUs].Min; selection of the recommended value (falling
// back to min + warning when recommended doesn't fit, then refusal when
// min doesn't fit either); and rejection of any user override outside
// [min, max] — no silent clamping, no silent substitution.
type ResourceProfile struct {
	// Service is the Compose service name this entry sizes
	// (e.g. "app", "postgres", "runner"). Must match a service
	// declared in the app's Compose template.
	Service string `yaml:"service" json:"service"`

	// Memory carries the memory band (min / recommended / max)
	// for this service.
	Memory MemoryBand `yaml:"memory" json:"memory"`

	// CPUs carries the CPU band (min / recommended / max) for
	// this service. Values are decimal CPU quotas (e.g. "0.25",
	// "1.0", "2.5").
	CPUs CPUBand `yaml:"cpus" json:"cpus"`

	// PIDs carries the pids defense-in-depth limits (default and
	// max) for this service. No min — pids is a containment
	// limit, not a sizing requirement.
	PIDs PIDsBand `yaml:"pids" json:"pids"`

	// AllowOverride, when false, makes `internal/core` install
	// planning reject any InstallRequest override targeting this
	// service with `*types.Error{Code: ErrCodeUsageValidation}`
	// rather than silently substituting the recommended value —
	// the user learns about the constraint instead of seeing an
	// it for services with hard runtime requirements where
	// undersizing would silently break the app.
	AllowOverride bool `yaml:"allow_override" json:"allow_override"`
}

// MemoryBand is a [ResourceProfile.Memory] sizing band. Values use
// Docker's `<integer><b|k|m|g>` format (e.g. "256m", "1g"); the schema
// rejects unit-less values so the catalog cannot ship an ambiguous "1"
// meaning 1 byte.
type MemoryBand struct {
	// Min is the floor: below this, wdm refuses install
	// with `*types.Error{Code: ErrCodeUsageValidation, Hint:
	// "host resources below minimum for <app>/<service>"}`
	// before writing files or calling Docker. PRD §27 reserves
	// exit codes 0–9 so host-resources-below-min reuses the
	// existing usage-validation code rather than introducing a
	// new one. Compared against host MemTotal minus the 1 GiB
	// OS-and-reverse-proxy reserve minus already-installed
	// stacks' recommended memory totals.
	Min string `yaml:"min" json:"min"`

	// Recommended is the normal default for a typical small VPS —
	// NOT the current maintainer machine's value. wdm selects it
	// when it fits the detected host memory budget; falls back to
	// [Min] with a warning (StepInstallResourceDegraded progress
	// step) if it doesn't fit but Min does; refuses install if Min
	// doesn't fit either. Drives the .env-rendered
	// MEMORY_LIMIT_<SERVICE_KEY> value Compose substitutes at up -d
	// time. SERVICE_KEY is derived from [ResourceProfile.Service]
	// per 's SERVICE_KEY derivation subsection.
	Recommended string `yaml:"recommended" json:"recommended"`

	// Max is the override ceiling AND a placeholder for an optional
	// future "large" profile — wdm does NOT auto-select Max as the
	// install default. User overrides above it are rejected with
	// `*types.Error{Code: ErrCodeUsageValidation}` — no silent
	// clamping. The catalog cap guards against fat-fingering (e.g. a
	// typo turning "2g" into "20g" that would over-commit the host).
	Max string `yaml:"max" json:"max"`
}

// CPUBand is a [ResourceProfile.CPUs] sizing band. Values use Docker's
// decimal-string CPU quota for fractional CPUs (e.g. "0.25", "1.0",
// "2.5"). The string form preserves the trailing zero, which YAML would
// otherwise collapse: "1.0" to the integer 1. The schema rejects
// all-zero magnitudes ("0", "0.0") since a zero CPU quota is
// nonsensical for a sizing band.
type CPUBand struct {
	// Min is the floor: below this, wdm refuses install
	// with `*types.Error{Code: ErrCodeUsageValidation, Hint:
	// "host resources below minimum for <app>/<service>"}` —
	// same install-time-validation framing as
	// [MemoryBand.Min] (no new exit code per PRD §27).
	// Compared against runtime.NumCPU minus already-installed
	// stacks' recommended cpu totals.
	Min string `yaml:"min" json:"min"`

	// Recommended is the normal default for a typical small VPS —
	// NOT the maintainer machine's value. Same fit-fallback-refuse
	// algorithm as [MemoryBand.Recommended]. Drives the
	// .env-rendered CPUS_LIMIT_<SERVICE_KEY> value. SERVICE_KEY is
	// derived from [ResourceProfile.Service] per 's
	// SERVICE_KEY derivation subsection.
	Recommended string `yaml:"recommended" json:"recommended"`

	// Max is the override ceiling AND future-large-profile
	// placeholder — NOT auto-selected as default. User overrides
	// above this are rejected, no silent clamping.
	Max string `yaml:"max" json:"max"`
}

// PIDsBand is a [ResourceProfile.PIDs] defense-in-depth band.
// pids is a containment cap (against fork bombs, runaway worker
// spawning), not a sizing requirement, so the catalog declares a
// default and a ceiling only — there is no min field, and wdm
// does NOT autoscale pids from host resources.
type PIDsBand struct {
	// Default is the catalog-fixed containment cap applied at
	// install. Not autoscaled from host resources — pids is
	// defense-in-depth, not a sizing requirement. Drives the
	// .env-rendered PIDS_LIMIT_<SERVICE_KEY> value Compose
	// substitutes at up -d time. SERVICE_KEY is derived from
	// [ResourceProfile.Service] per 's SERVICE_KEY
	// derivation subsection.
	Default int `yaml:"default" json:"default"`

	// Max is the override ceiling: user overrides above this
	// are rejected (no silent clamping). Catalog cap matches
	// the highest pids value the app legitimately needs under
	// load.
	Max int `yaml:"max" json:"max"`
}
