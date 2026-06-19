#!/bin/sh
set -eu

base_ref=${WDM_CATALOG_BASE_REF:-}

all_zero_ref() {
	case "$1" in
	"" | 0000000000000000000000000000000000000000)
		return 0
		;;
	*)
		return 1
		;;
	esac
}

ref_exists() {
	git rev-parse --verify "$1^{commit}" >/dev/null 2>&1
}

resolve_base_ref() {
	# A configured base ref may be unreachable: e.g. github.event.before
	# points at a pre-rebase tip that a force-push orphaned. Fetch once,
	# then fall back to a known branch so the gate stays robust rather than
	# hard-failing on a vanished commit.
	if ! all_zero_ref "$base_ref"; then
		if ref_exists "$base_ref"; then
			printf '%s\n' "$base_ref"
			return 0
		fi
		# Best-effort fetch: hosts only serve commits reachable from an
		# advertised ref, so a bare SHA orphaned by a force-push usually
		# stays unreachable and this fetch fails (fine; ref_exists below
		# drives the fallback). It still helps when the configured base is
		# a real but not-yet-fetched ref such as a branch name.
		if git fetch --quiet origin "$base_ref" 2>/dev/null && ref_exists "$base_ref"; then
			printf '%s\n' "$base_ref"
			return 0
		fi
		printf '%s\n' "catalog freshness: base ref not reachable, falling back: $base_ref" >&2
	fi

	for candidate in origin/dev dev origin/main main HEAD~1; do
		if ref_exists "$candidate"; then
			printf '%s\n' "$candidate"
			return 0
		fi
	done

	# No diff base can be established (e.g. shallow single-commit checkout).
	# Skip the diff with a non-failing notice instead of crashing the gate.
	return 1
}

catalog_generated_at() {
	awk -F'"' '/^generated_at:/ { print $2; found = 1; exit } END { exit found ? 0 : 1 }' "$1"
}

require_canonical_utc() {
	label=$1
	value=$2

	case "$value" in
	????-??-??T??:??:??Z)
		return 0
		;;
	*)
		printf '%s\n' "catalog freshness: $label generated_at must use YYYY-MM-DDTHH:MM:SSZ" >&2
		exit 1
		;;
	esac
}

base_ref=$(resolve_base_ref) || {
	printf '%s\n' "catalog freshness OK: no base ref available to diff against (skipping)"
	exit 0
}

if ! ref_exists "$base_ref"; then
	printf '%s\n' "catalog freshness: base ref not found: $base_ref" >&2
	exit 1
fi

merge_base=$(git merge-base HEAD "$base_ref") || {
	printf '%s\n' "catalog freshness: could not find merge base for $base_ref" >&2
	exit 1
}

changed_paths=$(git diff --name-only "$merge_base" -- catalog/stable/catalog.yaml templates)
if [ -z "$changed_paths" ]; then
	printf '%s\n' "catalog freshness OK: no catalog or template changes"
	exit 0
fi

base_manifest=$(mktemp)
trap 'rm -f "$base_manifest"' EXIT HUP INT TERM

if ! git show "$merge_base:catalog/stable/catalog.yaml" >"$base_manifest"; then
	printf '%s\n' "catalog freshness: base catalog manifest is unavailable" >&2
	exit 1
fi

base_generated_at=$(catalog_generated_at "$base_manifest") || {
	printf '%s\n' "catalog freshness: base catalog generated_at is missing" >&2
	exit 1
}
head_generated_at=$(catalog_generated_at catalog/stable/catalog.yaml) || {
	printf '%s\n' "catalog freshness: catalog generated_at is missing" >&2
	exit 1
}

require_canonical_utc base "$base_generated_at"
require_canonical_utc head "$head_generated_at"

oldest_generated_at=$(
	printf '%s\n%s\n' "$base_generated_at" "$head_generated_at" |
		LC_ALL=C sort |
		sed -n '1p'
)

if [ "$head_generated_at" = "$base_generated_at" ] ||
	[ "$oldest_generated_at" = "$head_generated_at" ]; then
	printf '%s\n' "catalog freshness: catalog/templates changed without advancing generated_at" >&2
	printf '%s\n' "base generated_at: $base_generated_at" >&2
	printf '%s\n' "head generated_at: $head_generated_at" >&2
	printf '%s\n' "changed paths:" >&2
	printf '%s\n' "$changed_paths" >&2
	exit 1
fi

printf '%s\n' "catalog freshness OK: $base_generated_at -> $head_generated_at"
