package security

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrUnsafePath is returned by the validators below when the supplied
// path is rejected outright — for example, the filesystem root or a
// known system directory (PRD §12 hardening rule: "Reject unsafe
// paths, including /, system directories, parent traversal, and
// root-owned stack paths").
// Errors that wrap this sentinel can be detected by callers with
// [errors.Is]; the wrapping context typically names the offending
// path and the reason it was rejected.
var ErrUnsafePath = errors.New("security: unsafe path")

// ErrPathEscape is returned when a candidate path, after lexical
// cleaning, resolves outside of the supplied root (PRD §12 hardening
// rule: "No writes outside the selected stack directory"). It is
// distinct from [ErrUnsafePath] so callers can distinguish "input
// itself is forbidden" from "input would escape its sandbox".
var ErrPathEscape = errors.New("security: path escapes root")

// unsafeRoots is the deny-list of absolute paths wdm must
// never treat as a managed stack base, a stack directory, or any other
// write destination. Each entry is matched case-sensitively against
// the cleaned absolute candidate; descendants of every entry (except
// "/") are also rejected.
// The list stays conservative to avoid false positives on user homes
// (/Users, /home) and on /opt, which some operators legitimately use.
var unsafeRoots = []string{
	"/",
	"/bin", "/sbin", "/boot",
	"/dev", "/proc", "/sys",
	"/etc",
	"/lib", "/lib32", "/lib64",
	"/usr",
	"/var",
	"/root",
}

// RejectUnsafeRoot returns [ErrUnsafePath] when path is empty, not
// absolute, or — after lexical cleaning — matches a known-dangerous
// system path (or any directory beneath one). The check is purely
// lexical (no [os.Stat], no symlink resolution), so it is safe to
// call during config validation before any directory exists on disk.
// path MUST be absolute: relative input is rejected because the
// caller has not yet resolved a base directory and the safety
// decision cannot be made without one. The function returns nil for
// safe paths such as /home/<user>/docker, /Users/<user>/docker, and
// /opt/<anything>.
func RejectUnsafeRoot(path string) error {
	if path == "" {
		return fmt.Errorf("%w: empty", ErrUnsafePath)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: %q is not absolute", ErrUnsafePath, path)
	}

	cleaned := filepath.Clean(path)
	for _, bad := range unsafeRoots {
		if cleaned == bad {
			return fmt.Errorf("%w: %q is a reserved system path", ErrUnsafePath, cleaned)
		}
		// "/" matches every absolute path as a prefix; the equality
		// check above is enough for it, so skip the descendant test.
		if bad == "/" {
			continue
		}
		prefix := bad + "/"
		if strings.HasPrefix(cleaned, prefix) {
			return fmt.Errorf("%w: %q is under reserved %q", ErrUnsafePath, cleaned, bad)
		}
	}
	return nil
}

// EnsureWithinRoot returns nil when candidate lies under root after
// lexical cleaning, and [ErrPathEscape] otherwise. Both inputs MUST
// be absolute; either being empty or relative produces
// [ErrUnsafePath] so a single [errors.Is] check at the call site
// covers both classes of rejection.
// The check is purely lexical and does not resolve symlinks. Callers
// needing symlink-aware containment for I/O must additionally open the
// directory through [os.Root] (Go 1.24+) at the moment of use.
// For config validation, the lexical check here is the contract.
func EnsureWithinRoot(root, candidate string) error {
	if root == "" || candidate == "" {
		return fmt.Errorf("%w: empty root or candidate", ErrUnsafePath)
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(candidate) {
		return fmt.Errorf("%w: root and candidate must be absolute", ErrUnsafePath)
	}

	cleanedRoot := filepath.Clean(root)
	cleanedCandidate := filepath.Clean(candidate)

	rel, err := filepath.Rel(cleanedRoot, cleanedCandidate)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPathEscape, err)
	}
	// [filepath.IsLocal] (Go 1.20+) is the canonical guard against
	// parent traversal, absolute-path re-injection, and Windows volume
	// references. It returns true for "." (root == candidate), so the
	// same-path case is accepted without a special branch.
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("%w: %q is outside %q", ErrPathEscape, cleanedCandidate, cleanedRoot)
	}
	return nil
}

// SafeJoin joins a trusted root with an untrusted relative subpath,
// returning the cleaned absolute result or an error. It is the
// recommended entry point when the caller has a known root (e.g. the
// configured stack base) and a sub-segment of unknown provenance
// (e.g. an app ID parsed from a catalog entry).
// root MUST be absolute. untrustedSub MUST be a local subpath as
// defined by [filepath.IsLocal] — non-local input (absolute paths,
// parent traversal, Windows volume references) is rejected with
// [ErrPathEscape] before [filepath.Join] is consulted, so the join
// can never silently produce a path outside root.
// SafeJoin re-runs [EnsureWithinRoot] on the joined result as
// defense in depth: even if the IsLocal contract regresses in a
// future Go release, the second check holds the security invariant.
func SafeJoin(root, untrustedSub string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%w: empty root", ErrUnsafePath)
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: root must be absolute", ErrUnsafePath)
	}
	if untrustedSub == "" {
		return "", fmt.Errorf("%w: empty subpath", ErrUnsafePath)
	}
	if !filepath.IsLocal(untrustedSub) {
		return "", fmt.Errorf("%w: %q is not a local subpath", ErrPathEscape, untrustedSub)
	}

	joined := filepath.Join(filepath.Clean(root), untrustedSub)
	if err := EnsureWithinRoot(root, joined); err != nil {
		return "", err
	}
	return joined, nil
}

// IsSuspiciouslyShallowPath reports whether cleaned — an absolute, cleaned
// path — sits at the filesystem root or a single top-level component such as
// "/etc" or "/home", and ONLY those. The floor is deliberately shallow: a
// two-segment path such as "/data/<app>" is NOT shallow and stays removable,
// because a single-segment custom stack base (e.g. "/data" resolving stacks
// to "/data/<app>") is legitimate and a stricter >=3-segment floor would
// refuse it. It is a defense-in-depth backstop that destructive verbs apply
// just before os.RemoveAll — a sibling of [RejectUnsafeRoot] and its
// unsafeRoots deny-list — not the load-bearing guard.
func IsSuspiciouslyShallowPath(cleaned string) bool {
	trimmed := strings.Trim(cleaned, string(filepath.Separator))
	if trimmed == "" {
		// The filesystem root.
		return true
	}
	// A single top-level component (no separator after trimming) is shallow.
	return !strings.Contains(trimmed, string(filepath.Separator))
}

// ResolveContainedPath resolves root and candidate through
// [filepath.EvalSymlinks] on BOTH sides, lexically cleans each result, and
// verifies the resolved candidate lies within the resolved root via
// [EnsureWithinRoot]. It is the symlink-aware containment seam destructive
// verbs run immediately before os.RemoveAll, so the comparison is
// symlink-consistent on both sides: comparing a resolved candidate against an
// unresolved root could spuriously pass or fail across a symlink indirection
// (e.g. /var -> /private/var on the test host).
//
// The errors are message-agnostic so each caller can translate them into its
// own PRD-anchored wording. Root is resolved first, then candidate, so the
// returned values identify which stage failed:
//   - empty resolvedRoot: the root failed to resolve;
//   - empty resolvedCandidate with non-empty resolvedRoot: the candidate
//     failed to resolve;
//   - both non-empty with err != nil: a containment escape ([ErrPathEscape]).
//
// EvalSymlinks errors pass through unwrapped, so a caller can map a
// non-existent candidate with [errors.Is](err, [os.ErrNotExist]).
func ResolveContainedPath(root, candidate string) (resolvedRoot, resolvedCandidate string, err error) {
	resolvedRoot, err = evalCleanPath(root)
	if err != nil {
		return "", "", err
	}
	resolvedCandidate, err = evalCleanPath(candidate)
	if err != nil {
		return resolvedRoot, "", err
	}
	if err := EnsureWithinRoot(resolvedRoot, resolvedCandidate); err != nil {
		return resolvedRoot, resolvedCandidate, err
	}
	return resolvedRoot, resolvedCandidate, nil
}

func evalCleanPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}
