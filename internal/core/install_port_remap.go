package core

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wnstify/wdm/internal/security"
)

// applyComposePortRemap rewrites the rendered compose host ports for the plan's
// PortOverrides and fails closed if any planned remap found no matching rendered
// binding (a catalog/template host-port drift would otherwise silently deploy on
// the original port). A no-override install is a no-op. Run before the §11.1
// bind scans so they validate the rewritten compose.
func applyComposePortRemap(plan *installPlan, redactor security.Redactor) error {
	if len(plan.portOverrides) == 0 {
		return nil
	}
	rewritten, matched, err := rewriteComposeHostPorts(plan.rendered.ComposeBytes, plan.portOverrides)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"remapped compose could not be produced",
			"retry without --port, or choose a different host port",
			err,
		)
	}
	for old := range plan.portOverrides {
		if _, ok := matched[old]; !ok {
			return redactedVerificationError(
				redactor,
				"remapped host port not found in rendered compose",
				"refresh the catalog and retry",
				fmt.Errorf("override host port %d has no matching loopback binding in the rendered compose", old),
			)
		}
	}
	plan.rendered.ComposeBytes = rewritten
	return nil
}

// rewriteComposeHostPorts edits the rendered docker-compose so a PortOverrides
// remap actually reaches the deployed binding. Catalog host ports are literal
// ints in each compose_template (no host-port placeholder exists), so changing
// the planned [types.PortBinding] alone does not move the bind — the rendered
// compose must be edited too (ADR 0004). For every ports entry whose published
// host port is an override key AND whose bind is loopback, the published port is
// rewritten old→new; the host IP, container port, and protocol are preserved.
//
// Only remappable bindings are touched: the short form keeps its
// host_ip:host:container shape (a 2-segment all-interfaces entry is public and
// is never remapped), the long form must carry a loopback host_ip, and a range
// published value (non-integer) is left alone. The caller runs the §11.1
// public/admin bind scans on the result, so a rewrite that somehow produced a
// non-loopback bind still fails closed there. Returns the input unchanged when
// no override applies, so a normal install renders byte-for-byte as before.
//
// matched reports which override host ports were found and rewritten in the
// compose, so the caller can fail closed if a planned remap never reached a
// rendered binding (a catalog/template host-port drift), instead of silently
// deploying on the original port.
func rewriteComposeHostPorts(composeBytes []byte, overrides map[int]int) (out []byte, matched map[int]struct{}, err error) {
	matched = map[int]struct{}{}
	if len(overrides) == 0 {
		return composeBytes, matched, nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(composeBytes, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse rendered compose for port remap: %w", err)
	}
	if len(doc.Content) == 0 {
		return composeBytes, matched, nil
	}

	services := mappingValue(doc.Content[0], "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return composeBytes, matched, nil
	}

	for i := 1; i < len(services.Content); i += 2 {
		ports := mappingValue(services.Content[i], "ports")
		if ports == nil || ports.Kind != yaml.SequenceNode {
			continue
		}
		for _, entry := range ports.Content {
			switch entry.Kind {
			case yaml.ScalarNode:
				if rewritten, old, ok := rewriteShortPort(entry.Value, overrides); ok {
					entry.Value = rewritten
					matched[old] = struct{}{}
				}
			case yaml.MappingNode:
				if old, ok := rewriteLongPort(entry, overrides); ok {
					matched[old] = struct{}{}
				}
			}
		}
	}

	if len(matched) == 0 {
		return composeBytes, matched, nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, nil, fmt.Errorf("re-encode remapped compose: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, nil, fmt.Errorf("close remapped compose encoder: %w", err)
	}
	return buf.Bytes(), matched, nil
}

// mappingValue returns the value node for key in a YAML mapping node, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// rewriteShortPort rewrites a Compose short-form port string when its loopback
// published port is an override key. Only the 3-segment host_ip:host:container
// form on a loopback host IP is remappable; a 2-segment (all-interfaces) entry
// and a non-integer (range) published value are left untouched.
func rewriteShortPort(value string, overrides map[int]int) (rewritten string, old int, ok bool) {
	spec, proto, hasProto := strings.Cut(value, "/")
	parts := strings.Split(spec, ":")
	if len(parts) != 3 || !isLoopbackHost(parts[0]) {
		return value, 0, false
	}
	host, err := strconv.Atoi(parts[1])
	if err != nil {
		return value, 0, false
	}
	newPort, found := overrides[host]
	if !found {
		return value, 0, false
	}
	parts[1] = strconv.Itoa(newPort)
	out := strings.Join(parts, ":")
	if hasProto {
		out += "/" + proto
	}
	return out, host, true
}

// rewriteLongPort rewrites a Compose long-form port mapping in place when its
// loopback published port is an override key. It returns the matched old host
// port and whether it changed the node.
func rewriteLongPort(node *yaml.Node, overrides map[int]int) (old int, ok bool) {
	hostIP := mappingValue(node, "host_ip")
	if hostIP == nil || !isLoopbackHost(hostIP.Value) {
		return 0, false
	}
	published := mappingValue(node, "published")
	if published == nil {
		return 0, false
	}
	host, err := strconv.Atoi(published.Value)
	if err != nil {
		return 0, false
	}
	newPort, found := overrides[host]
	if !found {
		return 0, false
	}
	published.Value = strconv.Itoa(newPort)
	return host, true
}

// isLoopbackHost reports whether a Compose host IP is a loopback address. An
// empty or non-loopback IP (0.0.0.0, a LAN address) is not remappable: a remap
// never turns a loopback port into a public one (ADR 0004 / PRD §11.1).
func isLoopbackHost(hostIP string) bool {
	ip := net.ParseIP(hostIP)
	return ip != nil && ip.IsLoopback()
}
