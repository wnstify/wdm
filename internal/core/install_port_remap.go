package core

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

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
func rewriteComposeHostPorts(composeBytes []byte, overrides map[int]int) ([]byte, error) {
	if len(overrides) == 0 {
		return composeBytes, nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(composeBytes, &doc); err != nil {
		return nil, fmt.Errorf("parse rendered compose for port remap: %w", err)
	}
	if len(doc.Content) == 0 {
		return composeBytes, nil
	}

	services := mappingValue(doc.Content[0], "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return composeBytes, nil
	}

	changed := false
	for i := 1; i < len(services.Content); i += 2 {
		ports := mappingValue(services.Content[i], "ports")
		if ports == nil || ports.Kind != yaml.SequenceNode {
			continue
		}
		for _, entry := range ports.Content {
			switch entry.Kind {
			case yaml.ScalarNode:
				if rewritten, ok := rewriteShortPort(entry.Value, overrides); ok {
					entry.Value = rewritten
					changed = true
				}
			case yaml.MappingNode:
				if rewriteLongPort(entry, overrides) {
					changed = true
				}
			}
		}
	}

	if !changed {
		return composeBytes, nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("re-encode remapped compose: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close remapped compose encoder: %w", err)
	}
	return buf.Bytes(), nil
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
func rewriteShortPort(value string, overrides map[int]int) (string, bool) {
	spec, proto, hasProto := strings.Cut(value, "/")
	parts := strings.Split(spec, ":")
	if len(parts) != 3 || !isLoopbackHost(parts[0]) {
		return value, false
	}
	host, err := strconv.Atoi(parts[1])
	if err != nil {
		return value, false
	}
	newPort, ok := overrides[host]
	if !ok {
		return value, false
	}
	parts[1] = strconv.Itoa(newPort)
	out := strings.Join(parts, ":")
	if hasProto {
		out += "/" + proto
	}
	return out, true
}

// rewriteLongPort rewrites a Compose long-form port mapping in place when its
// loopback published port is an override key. It returns whether it changed the
// node.
func rewriteLongPort(node *yaml.Node, overrides map[int]int) bool {
	hostIP := mappingValue(node, "host_ip")
	if hostIP == nil || !isLoopbackHost(hostIP.Value) {
		return false
	}
	published := mappingValue(node, "published")
	if published == nil {
		return false
	}
	host, err := strconv.Atoi(published.Value)
	if err != nil {
		return false
	}
	newPort, ok := overrides[host]
	if !ok {
		return false
	}
	published.Value = strconv.Itoa(newPort)
	return true
}

// isLoopbackHost reports whether a Compose host IP is a loopback address. An
// empty or non-loopback IP (0.0.0.0, a LAN address) is not remappable: a remap
// never turns a loopback port into a public one (ADR 0004 / PRD §11.1).
func isLoopbackHost(hostIP string) bool {
	ip := net.ParseIP(hostIP)
	return ip != nil && ip.IsLoopback()
}
