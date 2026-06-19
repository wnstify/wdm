#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
	printf '%s\n' "usage: extract-release-notes.sh CHANGELOG VERSION OUTPUT" >&2
	exit 2
fi

changelog=$1
version=$2
output=$3

# Strip HTML comments so internal notes never leak into public release notes.
# Multi-line comment blocks are dropped via the in_comment state. On the print
# path, inline comment spans are stripped from each line, including comments
# that share a line with visible text (e.g. "text <!-- note -->" or
# "<!-- note --> text"); a line that becomes whitespace-only after stripping is
# dropped, matching the old behavior of dropping pure comment lines. Lines with
# no comment print unchanged, including genuine blank lines.
#
# Accepted limitation: a version header ("## ...") embedded inside an HTML
# comment is treated as inert, so the active section is not split there. This
# matches treating commented-out content as ignored, and real changelogs do not
# embed headers in comments.
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
	in_section {
		line = $0
		if (line ~ /<!--/) {
			gsub(/<!--.*-->/, "", line)
			if (line ~ /^[[:space:]]*$/) {
				next
			}
		}
		print line
	}
' "$changelog" >"$output"

if ! grep -q '[^[:space:]]' "$output"; then
	printf '%s\n' "release notes are empty or missing for $version" >&2
	exit 1
fi
