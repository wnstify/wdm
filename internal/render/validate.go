package render

import (
	"fmt"
	"sort"
)

// ValidatePlaceholders checks a placeholder set for structural
// well-formedness: every [Placeholder.Name] non-empty and unique, and
// every [Placeholder.Type] one of the closed enum values in types.go.
// Placeholder VALUES are not checked here — path safety, domain
// syntax, port range, IANA timezone membership, and the like run in
// internal/core before render is called. This keeps the render boundary
// pure; value validation belongs to the orchestrator.
// Returns nil on success, otherwise an error wrapping
// [ErrPlaceholderNameEmpty], [ErrPlaceholderTypeInvalid], or
// [ErrPlaceholderNameDuplicate]. Error messages name the offending
// placeholder by [Placeholder.Name] but never echo
// [Placeholder.Default] or any other resolved value.
func ValidatePlaceholders(placeholders []Placeholder) error {
	seen := make(map[string]struct{}, len(placeholders))
	for i, p := range placeholders {
		if p.Name == "" {
			return fmt.Errorf("render: placeholder at index %d has empty name: %w", i, ErrPlaceholderNameEmpty)
		}
		if !p.Type.IsValid() {
			return fmt.Errorf("render: placeholder %q has invalid type %q: %w", p.Name, string(p.Type), ErrPlaceholderTypeInvalid)
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("render: placeholder %q declared more than once: %w", p.Name, ErrPlaceholderNameDuplicate)
		}
		seen[p.Name] = struct{}{}
	}
	return nil
}

// ValidateResolution checks that resolved exactly matches the declared
// placeholder set: every Required [Placeholder] has a key in resolved,
// and resolved carries no keys outside the set. Non-required
// placeholders may be absent (their [Placeholder.Default] applies at
// render time).
// Deterministic: missing-required checks run in placeholder slice
// order, and extra-key checks run over a sorted copy of resolved's
// keys, so the reported key is stable across Go versions and hardware.
// Error messages name the offending placeholder by [Placeholder.Name]
// or the offending resolution key but NEVER include the resolved
// value — treats every resolved value as potentially
// sensitive (some are secrets unredacted at this layer).
// Returns nil when resolved matches, otherwise an error wrapping
// [ErrResolutionMissingPlaceholder] or [ErrResolutionExtraKey].
func ValidateResolution(placeholders []Placeholder, resolved map[string]string) error {
	declared := make(map[string]bool, len(placeholders))
	for _, p := range placeholders {
		declared[p.Name] = true
		if p.Required {
			if _, ok := resolved[p.Name]; !ok {
				return fmt.Errorf("render: required placeholder %q absent from resolution: %w", p.Name, ErrResolutionMissingPlaceholder)
			}
		}
	}

	keys := make([]string, 0, len(resolved))
	for k := range resolved {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !declared[k] {
			return fmt.Errorf("render: resolution key %q not declared in placeholders: %w", k, ErrResolutionExtraKey)
		}
	}
	return nil
}
