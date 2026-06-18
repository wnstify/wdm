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

## v1.0.2 - 2026-06-18

<!-- internal note -->
### Fixed
- Advanced the stable catalog version.

## v1.0.1 - 2026-06-18

### Fixed
- Previous release note.
EOF
}

expect_pass() {
	name=$1
	version=$2
	want=$3
	out="$tmp_root/$name.md"
	err="$tmp_root/$name.err"
	changelog="$tmp_root/$name-CHANGELOG.md"

	write_changelog "$changelog"
	if sh "$extract" "$changelog" "$version" "$out" 2>"$err" &&
		contains "$out" "$want" &&
		! contains "$out" "Previous release note" &&
		! contains "$out" "<!-- internal note -->"; then
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

extract_to() {
	changelog=$1
	version=$2
	out=$3
	err=$4

	sh "$extract" "$changelog" "$version" "$out" 2>"$err"
}

report_fail() {
	name=$1
	out=$2
	err=$3

	printf '%s\n' "not ok - $name" >&2
	printf '%s\n' "stdout:" >&2
	sed -n '1,40p' "$out" >&2 2>/dev/null || true
	printf '%s\n' "stderr:" >&2
	sed -n '1,40p' "$err" >&2 2>/dev/null || true
	failures=$((failures + 1))
}

write_prefix_changelog() {
	cat >"$1" <<'EOF'
# Changelog

## v1.0.10 - 2026-06-18

### Fixed
- Ten-patch body line.

## v1.0.1 - 2026-06-18

### Fixed
- One-patch body line.
EOF
}

write_no_date_changelog() {
	cat >"$1" <<'EOF'
# Changelog

## v2.0.0

### Added
- Major release body line.
EOF
}

write_multiline_comment_changelog() {
	cat >"$1" <<'EOF'
# Changelog

## v3.0.0 - 2026-06-18

<!-- single-line secret -->
<!--
multi-line secret line one
multi-line secret line two
-->
### Fixed
- Public release body line.
EOF
}

prefix_changelog="$tmp_root/prefix-CHANGELOG.md"
write_prefix_changelog "$prefix_changelog"

out="$tmp_root/prefix-low.md"
err="$tmp_root/prefix-low.err"
if extract_to "$prefix_changelog" "v1.0.1" "$out" "$err" &&
	contains "$out" "One-patch body line" &&
	! contains "$out" "Ten-patch body line"; then
	printf '%s\n' "ok - prefix v1.0.1 does not capture v1.0.10"
else
	report_fail "prefix v1.0.1 does not capture v1.0.10" "$out" "$err"
fi

out="$tmp_root/prefix-high.md"
err="$tmp_root/prefix-high.err"
if extract_to "$prefix_changelog" "v1.0.10" "$out" "$err" &&
	contains "$out" "Ten-patch body line" &&
	! contains "$out" "One-patch body line"; then
	printf '%s\n' "ok - prefix v1.0.10 does not capture v1.0.1"
else
	report_fail "prefix v1.0.10 does not capture v1.0.1" "$out" "$err"
fi

no_date_changelog="$tmp_root/no-date-CHANGELOG.md"
write_no_date_changelog "$no_date_changelog"

out="$tmp_root/no-date.md"
err="$tmp_root/no-date.err"
if extract_to "$no_date_changelog" "v2.0.0" "$out" "$err" &&
	contains "$out" "Major release body line"; then
	printf '%s\n' "ok - date-suffix-less header extracts"
else
	report_fail "date-suffix-less header extracts" "$out" "$err"
fi

comment_changelog="$tmp_root/comment-CHANGELOG.md"
write_multiline_comment_changelog "$comment_changelog"

out="$tmp_root/comment.md"
err="$tmp_root/comment.err"
if extract_to "$comment_changelog" "v3.0.0" "$out" "$err" &&
	contains "$out" "Public release body line" &&
	! contains "$out" "single-line secret" &&
	! contains "$out" "multi-line secret line one" &&
	! contains "$out" "multi-line secret line two"; then
	printf '%s\n' "ok - multi-line and single-line comments stripped"
else
	report_fail "multi-line and single-line comments stripped" "$out" "$err"
fi

expect_pass "extracts requested release section" "v1.0.2" "Advanced the stable catalog version"
expect_fail "missing release section fails closed" "v9.9.9" "release notes are empty or missing for v9.9.9"

[ "$failures" -eq 0 ]
