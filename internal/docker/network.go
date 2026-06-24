package docker

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/wnstify/wdm/pkg/types"
)

var networkNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// missingNetworkIndicator is the classic missing-network phrasing emitted by
// older Docker daemons and CLI versions (e.g. "No such network: <name>").
const missingNetworkIndicator = "no such network"

// NetworkSpec declares one Docker network requirement from catalog
// install/update planning.
type NetworkSpec struct {
	Name     string
	Internal bool

	// Subnet, when set, is the IPv4 CIDR passed as --subnet to
	// `docker network create` so the network's addressing is fixed (PRD §9).
	// Empty leaves Docker's default bridge addressing unchanged.
	Subnet string

	// Gateway, when set, is the IPv4 gateway passed as --gateway to
	// `docker network create`. It is only meaningful alongside a Subnet and
	// must fall within it. Empty lets Docker pick the subnet's first address.
	Gateway string

	// AppID, when set, stamps the wdm ownership labels (PRD §10) onto a
	// newly-created network: `--label wdm.managed=true --label wdm.app=<AppID>`.
	// It must be the app's canonical catalog ID. Empty leaves the create
	// command label-free. Labels are applied only to networks wdm creates here;
	// a network reached through the EnsureNetworkReport "already exists" path is NOT
	// re-labeled (an accepted limitation — re-stamping existing networks is the
	// deferred label-sweep's job, out of scope for the create path).
	AppID string
}

// managedNetworkLabel and appNetworkLabelPrefix are the PRD §10 ownership
// labels stamped onto wdm-created networks. They are emitted in this fixed
// order (`wdm.managed=true` then `wdm.app=<appID>`) so the create argv is
// deterministic and the last-gate validator can match the pair positionally.
const (
	managedNetworkLabel   = "wdm.managed=true"
	appNetworkLabelPrefix = "wdm.app="
)

// managedNetworkLabelFilter and managedNetworkListFormat are the fixed tokens
// of the label-filtered managed-network list (`network ls --filter
// label=wdm.managed=true --format {{.Name}}`). They are constants so the
// builder and the last-gate validator match the exact same literals — the list
// is the discovery half of the self-uninstall network sweep (PRD §39), finding
// every wdm.managed=true network including ones orphaned by an app the operator
// already deleted.
const (
	managedNetworkLabelFilter = "label=" + managedNetworkLabel
	managedNetworkListFormat  = "{{.Name}}"
)

// EnsureNetworkReport ensures one network exists before compose deployment.
// If it already exists, the internal flag must match exactly and, when the spec
// pins a subnet (PRD §9), the existing subnet must match too; either mismatch is
// a fail-closed usage-validation error. A missing network is created with the
// requested --internal/--subnet/--gateway flags.
//
// It also reports whether the network was newly created. created is true only
// when a missing network was created on this call; it is false when the network
// already existed and was reconciled, and false on every error path — so created
// is meaningful only when err is nil. The created bool exists so install
// rollback can distinguish the networks it created from pre-existing ones and
// clean up only its own (PRD §9).
func EnsureNetworkReport(ctx context.Context, client Client, network NetworkSpec) (created bool, err error) {
	if client == nil {
		return false, types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	normalized, err := validateNetworkSpec(network)
	if err != nil {
		return false, err
	}

	inspectRes, inspectErr := client.Run(
		ctx,
		networkInspectInvocation{name: normalized.Name},
	)
	if inspectErr == nil {
		existingInternal, parseErr := parseNetworkInternalFlag(inspectRes.Stdout, normalized.Name)
		if parseErr != nil {
			return false, parseErr
		}
		if existingInternal != normalized.Internal {
			return false, types.NewError(
				types.ErrCodeUsageValidation,
				"network internal flag mismatch",
				fmt.Sprintf(
					"network %s exists with mismatched internal flag",
					normalized.Name,
				),
			)
		}
		return false, verifyExistingNetworkSubnet(ctx, client, normalized)
	}

	if !isMissingNetworkError(inspectRes, inspectErr, normalized.Name) {
		return false, inspectErr
	}

	_, createErr := client.Run(
		ctx,
		networkCreateInvocation{
			name:     normalized.Name,
			internal: normalized.Internal,
			subnet:   normalized.Subnet,
			gateway:  normalized.Gateway,
			appID:    normalized.AppID,
		},
	)
	if createErr != nil {
		return false, createErr
	}

	return true, nil
}

// RemoveNetwork removes a single network by name. It is the failure-rollback
// counterpart to the create path in [EnsureNetworkReport]: an install that
// created a network and then failed can hand that name back here to remove it.
// The name is validated by the same strict validator the create path uses, so
// only a well-formed network name reaches the daemon (PRD §12). It removes
// exactly one named network — no prune, no force, no wildcard.
func RemoveNetwork(ctx context.Context, client Client, networkName string) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	name, err := validateNetworkName(networkName)
	if err != nil {
		return err
	}

	_, err = client.Run(ctx, removeNetworkInvocation{name: name})
	return err
}

// RemoveNetworkIfPresent removes a single network by name like [RemoveNetwork]
// but treats an already-absent network as success (idempotent). It exists for
// the self-uninstall best-effort network cleanup (PRD §39), where a network the
// teardown already dropped — or one a previous run removed — must not surface as
// an error. A genuine removal failure (for example a network still holding
// endpoints) propagates unchanged so the caller can record it and continue. The
// not-found tolerance is local to this seam; [RemoveNetwork]'s other callers
// keep failing closed on every error.
func RemoveNetworkIfPresent(ctx context.Context, client Client, networkName string) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	name, err := validateNetworkName(networkName)
	if err != nil {
		return err
	}

	res, err := client.Run(ctx, removeNetworkInvocation{name: name})
	if err == nil {
		return nil
	}
	if isMissingNetworkError(res, err, name) {
		return nil
	}
	return err
}

// RemoveNetworkIfManaged removes a single network by name like
// [RemoveNetworkIfPresent], but ONLY after confirming the network carries the
// `wdm.managed=true` ownership label (PRD §10). It exists for the compose-
// derived delete/uninstall cleanup, where a stack's rendered compose declares
// its networks external by name: that name could, after the stack is gone,
// resolve to a foreign network an operator created. Inspecting the label before
// removal keeps the cleanup from deleting a network wdm does not own. A network
// already absent counts as success (idempotent). A network present but NOT
// carrying the label is left in place and reported via skipped=true with no
// error. Any other inspect/removal fault propagates unchanged. removed is true
// only when an owned network was actually removed.
func RemoveNetworkIfManaged(ctx context.Context, client Client, networkName string) (removed, skipped bool, err error) {
	if client == nil {
		return false, false, types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	name, err := validateNetworkName(networkName)
	if err != nil {
		return false, false, err
	}

	res, inspectErr := client.Run(ctx, networkManagedLabelInvocation{name: name})
	if inspectErr != nil {
		// An absent network is nothing to remove: idempotent success, not owned.
		if isMissingNetworkError(res, inspectErr, name) {
			return false, false, nil
		}
		return false, false, inspectErr
	}
	if strings.TrimRight(res.Stdout, "\r\n") != "true" {
		// Present but not wdm-owned: leave it alone.
		return false, true, nil
	}

	if removeErr := RemoveNetworkIfPresent(ctx, client, name); removeErr != nil {
		return false, false, removeErr
	}
	return true, false, nil
}

// ListManagedNetworks returns the names of every Docker network carrying the
// `wdm.managed=true` label, including ones orphaned by an app whose stack the
// operator already deleted (its compose file is gone, so the compose-derived
// network discovery can no longer find them). It is the discovery half of the
// self-uninstall network sweep (PRD §39): the names feed the same best-effort
// [RemoveNetworkIfPresent] cleanup, so a leftover labeled network no longer
// survives an uninstall. The list is read through a strictly-allowlisted
// `network ls --filter label=wdm.managed=true --format {{.Name}}` invocation;
// names are parsed one per line, trimmed, with blank lines dropped. A daemon
// failure propagates so the caller can treat it as a non-fatal cleanup
// degradation and continue.
func ListManagedNetworks(ctx context.Context, client Client) ([]string, error) {
	if client == nil {
		return nil, types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	res, err := client.Run(ctx, managedNetworkListInvocation{})
	if err != nil {
		return nil, err
	}

	return parseManagedNetworkNames(res.Stdout), nil
}

// parseManagedNetworkNames splits the `network ls --format {{.Name}}` output
// into one network name per line, trimming surrounding whitespace and dropping
// blank lines. Docker emits one name per line with a trailing newline; an empty
// output (no managed networks) yields an empty slice. Unlike the strict inspect
// parsers this tolerates blank lines rather than failing closed, because the
// list only feeds the best-effort sweep — each name is still re-validated by
// the strict network-name validator before any `network rm` reaches the daemon.
func parseManagedNetworkNames(stdout string) []string {
	names := []string{}
	for line := range strings.Lines(stdout) {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// verifyExistingNetworkSubnet refuses an existing network whose configured
// subnet does not match the requested spec, the same fail-closed shape as the
// internal-flag mismatch. When the spec pins no subnet there is nothing to
// reconcile, so any existing addressing is accepted.
func verifyExistingNetworkSubnet(ctx context.Context, client Client, network NetworkSpec) error {
	if network.Subnet == "" {
		return nil
	}

	subnetRes, subnetErr := client.Run(
		ctx,
		networkSubnetInvocation{name: network.Name},
	)
	if subnetErr != nil {
		return subnetErr
	}
	existingSubnet := strings.TrimRight(subnetRes.Stdout, "\r\n")
	if existingSubnet == network.Subnet {
		return nil
	}
	return types.NewError(
		types.ErrCodeUsageValidation,
		"network subnet mismatch",
		fmt.Sprintf(
			"network %s exists with mismatched subnet",
			network.Name,
		),
	)
}

type networkInspectInvocation struct {
	name string
}

func (networkInspectInvocation) isDockerInvocation() {}

// networkManagedLabelInvocation reads a network's `wdm.managed` label so
// [RemoveNetworkIfManaged] can gate compose-derived removal on wdm ownership
// (PRD §10). The format prints the label value or empty when absent.
type networkManagedLabelInvocation struct {
	name string
}

func (networkManagedLabelInvocation) isDockerInvocation() {}

// networkSubnetInvocation reads an existing network's first configured subnet so
// [EnsureNetworkReport] can reconcile it against the requested spec on the exists path.
type networkSubnetInvocation struct {
	name string
}

func (networkSubnetInvocation) isDockerInvocation() {}

type networkCreateInvocation struct {
	name     string
	internal bool
	subnet   string
	gateway  string

	// appID, when non-empty, stamps the PRD §10 ownership labels onto the
	// created network. It is the app's canonical catalog ID, validated by the
	// same charset the catalog schema enforces.
	appID string
}

func (networkCreateInvocation) isDockerInvocation() {}

// removeNetworkInvocation maps to `network rm <name>` so [RemoveNetwork] can
// drop a single network during install-failure rollback.
type removeNetworkInvocation struct {
	name string
}

func (removeNetworkInvocation) isDockerInvocation() {}

// managedNetworkListInvocation maps to the label-filtered managed-network list
// (`network ls --filter label=wdm.managed=true --format {{.Name}}`) so
// [ListManagedNetworks] can discover every wdm.managed=true network, including
// orphaned ones, for the self-uninstall sweep (PRD §39). It carries no fields:
// the filter and format are fixed literals.
type managedNetworkListInvocation struct{}

func (managedNetworkListInvocation) isDockerInvocation() {}

func validateNetworkSpec(network NetworkSpec) (NetworkSpec, error) {
	name, err := validateNetworkName(network.Name)
	if err != nil {
		return NetworkSpec{}, err
	}

	subnet, gateway, err := validateNetworkAddressing(network.Subnet, network.Gateway)
	if err != nil {
		return NetworkSpec{}, err
	}

	appID, err := validateNetworkAppID(network.AppID)
	if err != nil {
		return NetworkSpec{}, err
	}

	return NetworkSpec{
		Name:     name,
		Internal: network.Internal,
		Subnet:   subnet,
		Gateway:  gateway,
		AppID:    appID,
	}, nil
}

// networkAppIDPattern mirrors the catalog app_id schema (PRD §9, §17):
// lowercase ASCII letter first, then lowercase letters, digits, or hyphen,
// length 1-63. It is the charset stamped into the `wdm.app=<appID>` label, so
// only a well-formed app ID can ever reach the create argv or the daemon.
var networkAppIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// validateNetworkAppID accepts an empty app ID (the create command is then
// label-free) and otherwise requires the canonical catalog app_id charset. The
// labels are stamped from this value, so validating it here keeps a malformed
// or injection-bearing ID out of the argv (PRD §10, §12).
func validateNetworkAppID(rawAppID string) (string, error) {
	if rawAppID == "" {
		return "", nil
	}
	if !networkAppIDPattern.MatchString(rawAppID) {
		return "", types.WrapError(
			types.ErrCodeUsageValidation,
			"network app id is invalid",
			"use the catalog app_id: lowercase ascii starting with a letter, then lowercase letters/digits/hyphen, length 1-63",
			fmt.Errorf("network app id %q does not match allowed format", rawAppID),
		)
	}
	return rawAppID, nil
}

// validateNetworkAddressing validates the optional static-addressing fields: a
// set subnet must be a well-formed IPv4 CIDR, and a set gateway must be a valid
// IPv4. A gateway without a subnet is refused — Docker rejects --gateway absent
// --subnet, so catching it here keeps the failure a usage-validation error
// rather than an opaque daemon error. The returned values are the canonical
// netip forms so the create argv and the exists-path subnet comparison use one
// normalized representation.
func validateNetworkAddressing(subnet, gateway string) (string, string, error) {
	if subnet == "" {
		if gateway != "" {
			return "", "", types.WrapError(
				types.ErrCodeUsageValidation,
				"network gateway requires a subnet",
				"declare ipam.subnet alongside the gateway",
				fmt.Errorf("gateway %q has no subnet", gateway),
			)
		}
		return "", "", nil
	}

	prefix, err := netip.ParsePrefix(subnet)
	if err != nil || !prefix.Addr().Is4() {
		return "", "", types.WrapError(
			types.ErrCodeUsageValidation,
			"network subnet is invalid",
			"use an IPv4 CIDR such as 10.0.0.0/24",
			fmt.Errorf("subnet %q is not a valid IPv4 CIDR", subnet),
		)
	}
	normalizedSubnet := prefix.Masked().String()

	if gateway == "" {
		return normalizedSubnet, "", nil
	}
	gatewayAddr, err := netip.ParseAddr(gateway)
	if err != nil || !gatewayAddr.Is4() {
		return "", "", types.WrapError(
			types.ErrCodeUsageValidation,
			"network gateway is invalid",
			"use an IPv4 address within the subnet",
			fmt.Errorf("gateway %q is not a valid IPv4 address", gateway),
		)
	}
	return normalizedSubnet, gatewayAddr.String(), nil
}

func validateNetworkName(rawName string) (string, error) {
	if strings.TrimSpace(rawName) == "" {
		return "", types.NewError(
			types.ErrCodeUsageValidation,
			"network name is required",
			"use lowercase ascii, start with a letter, then lowercase letters/digits/underscore/hyphen, length 1-63",
		)
	}

	if !networkNamePattern.MatchString(rawName) {
		return "", types.WrapError(
			types.ErrCodeUsageValidation,
			"network name is invalid",
			"use lowercase ascii, start with a letter, then lowercase letters/digits/underscore/hyphen, length 1-63",
			fmt.Errorf("network name %q does not match allowed format", rawName),
		)
	}

	return rawName, nil
}

func parseNetworkInternalFlag(stdout, networkName string) (bool, error) {
	value := strings.TrimSuffix(stdout, "\n")
	value = strings.TrimSuffix(value, "\r")

	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, types.WrapError(
			types.ErrCodeUsageValidation,
			"network inspect output is invalid",
			"inspect output for network internal flag must be exactly true or false",
			fmt.Errorf(
				"network %s inspect returned unexpected internal flag output %q",
				networkName,
				stdout,
			),
		)
	}
}

// isMissingNetworkError reports whether an inspect failure means the
// network is absent (so [EnsureNetworkReport] should create it) rather than
// another fault that must propagate unchanged. It recognizes two daemon
// phrasings: the classic "no such network" form and the modern
// "network <name> not found" form from Docker 29.x (observed on the dev
// VM). The modern phrase is anchored to the network name so an unrelated
// "not found" error — for example `exec: "docker": executable file not
// found in $PATH` — never trips the create path. Anything unrecognized
// fails closed and propagates.
func isMissingNetworkError(res CommandResult, err error, networkName string) bool {
	if err == nil {
		return false
	}

	modernIndicator := "network " + strings.ToLower(networkName) + " not found"

	lowerStderr := strings.ToLower(res.Stderr)
	if strings.Contains(lowerStderr, missingNetworkIndicator) ||
		strings.Contains(lowerStderr, modernIndicator) {
		return true
	}

	lowerCause := strings.ToLower(err.Error())
	return strings.Contains(lowerCause, missingNetworkIndicator) ||
		strings.Contains(lowerCause, modernIndicator)
}
