#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
	printf '%s\n' "usage: extract-release-notes.sh CHANGELOG VERSION OUTPUT" >&2
	exit 2
fi

changelog=$1
version=$2
output=$3

# Strip both single-line (<!-- x -->) and multi-line HTML comment blocks so
# internal notes never leak into public release notes. Comment handling runs
# before the section toggle so a comment line never opens, closes, or prints a
# section.
awk -v version="$version" '
	in_comment {
		if (index($0, "-->")) {
			in_comment = 0
		}
		next
	}
	/<!--/ && $0 !~ /-->/ {
		in_comment = 1
		next
	}
	$0 == "## " version || index($0, "## " version " ") == 1 {
		in_section = 1
		next
	}
	in_section && /^## / {
		in_section = 0
	}
	in_section && $0 !~ /^<!--.*-->$/ {
		print
	}
' "$changelog" >"$output"

if ! grep -q '[^[:space:]]' "$output"; then
	printf '%s\n' "release notes are empty or missing for $version" >&2
	exit 1
fi
