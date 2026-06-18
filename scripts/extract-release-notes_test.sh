#!/bin/sh
set -eu

script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd)
extract="$script_dir/extract-release-notes.sh"

failures=0
tmp_root=$(mktemp -d)
trap 'rm -r "$tmp_root"' EXIT HUP INT TERM

contains() {
	file=$1
	needle=$2

	grep -Fq "$needle" "$file"
}

write_changelog() {
	path=$1

	cat >"$path" <<'EOF'
# Changelog

## v1.0.10 - 2026-06-18

### Fixed
- Future prefix release note.

## v1.0.2 - 2026-06-18

<!-- internal note -->
<!--
multi-line internal note
-->
### Fixed
- Advanced the stable catalog version.

## v1.0.1 - 2026-06-18

### Fixed
- Previous release note.

## v2.0.0

### Fixed
- No-date release note.
EOF
}

expect_pass() {
	name=$1
	version=$2
	want=$3
	reject=${4:-}
	out="$tmp_root/$name.md"
	err="$tmp_root/$name.err"
	changelog="$tmp_root/$name-CHANGELOG.md"

	write_changelog "$changelog"
	if sh "$extract" "$changelog" "$version" "$out" 2>"$err" &&
		contains "$out" "$want" &&
		! contains "$out" "<!-- internal note -->" &&
		! contains "$out" "multi-line internal note" &&
		{ [ -z "$reject" ] || ! contains "$out" "$reject"; }; then
		printf '%s\n' "ok - $name"
		return
	fi

	printf '%s\n' "not ok - $name" >&2
	printf '%s\n' "stdout:" >&2
	sed -n '1,40p' "$out" >&2 2>/dev/null || true
	printf '%s\n' "stderr:" >&2
	sed -n '1,40p' "$err" >&2 2>/dev/null || true
	failures=$((failures + 1))
}

expect_fail() {
	name=$1
	version=$2
	want=$3
	out="$tmp_root/$name.md"
	err="$tmp_root/$name.err"
	changelog="$tmp_root/$name-CHANGELOG.md"

	write_changelog "$changelog"
	if sh "$extract" "$changelog" "$version" "$out" 2>"$err"; then
		printf '%s\n' "not ok - $name: extractor passed" >&2
		failures=$((failures + 1))
		return
	fi
	if contains "$err" "$want"; then
		printf '%s\n' "ok - $name"
		return
	fi

	printf '%s\n' "not ok - $name: missing expected error" >&2
	sed -n '1,40p' "$err" >&2
	failures=$((failures + 1))
}

expect_pass "extracts requested release section" "v1.0.2" "Advanced the stable catalog version" "Previous release note"
expect_pass "version prefix does not leak" "v1.0.1" "Previous release note" "Future prefix release note"
expect_pass "extracts release section without date suffix" "v2.0.0" "No-date release note" "Advanced the stable catalog version"
expect_fail "missing release section fails closed" "v9.9.9" "release notes are empty or missing for v9.9.9"

[ "$failures" -eq 0 ]
