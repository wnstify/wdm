#!/usr/bin/env bash
# Offline coverage gate.

set -euo pipefail

GO_BIN="${GO:-go}"
PROFILE="${WDM_COVERAGE_PROFILE:-/tmp/wdm-coverage.out}"
FILTERED_PROFILE="${WDM_COVERAGE_FILTERED_PROFILE:-/tmp/wdm-coverage-filtered.out}"
SUMMARY="${WDM_COVERAGE_SUMMARY:-/tmp/wdm-coverage-summary.txt}"
MINIMUM="${WDM_COVERAGE_MIN:-80.0}"
# Only cmd/wdm is excluded from the coverage gate: it is the os.Exit-calling
# main package whose run/main paths cannot be unit-tested without process
# control, and the exit-code mapping (PRD §27) is reserved for cmd/wdm alone.
IGNORE_PATHS="cmd/wdm"

module="$("$GO_BIN" list -m)"

mkdir -p "$(dirname "$PROFILE")" "$(dirname "$FILTERED_PROFILE")" "$(dirname "$SUMMARY")"

printf "Running coverage gate with race detector...\n"
"$GO_BIN" test -count=1 -race -shuffle=on -coverprofile="$PROFILE" ./...

printf "Filtering coverage-ignored paths: %s\n" "$IGNORE_PATHS"
awk -v module="$module" -v ignores="$IGNORE_PATHS" '
BEGIN {
    ignore_count = split(ignores, ignore_paths, " ")
}
NR == 1 {
    print
    next
}
{
    skip = 0
    for (i = 1; i <= ignore_count; i++) {
        prefix = module "/" ignore_paths[i] "/"
        if (index($0, prefix) == 1) {
            skip = 1
            break
        }
    }
    if (!skip) {
        print
    }
}
' "$PROFILE" > "$FILTERED_PROFILE"

"$GO_BIN" tool cover -func="$FILTERED_PROFILE" | tee "$SUMMARY"

coverage="$(awk '/^total:/ { gsub(/%/, "", $3); print $3 }' "$SUMMARY")"
if [[ -z "$coverage" ]]; then
    printf "coverage gate: failed to read total coverage from %s\n" "$SUMMARY" >&2
    exit 1
fi

if ! awk -v coverage="$coverage" -v minimum="$MINIMUM" 'BEGIN { exit !(coverage + 0 >= minimum + 0) }'; then
    printf "coverage gate: %.1f%% is below %.1f%%\n" "$coverage" "$MINIMUM" >&2
    exit 1
fi

printf "coverage gate: %.1f%% >= %.1f%%\n" "$coverage" "$MINIMUM"
printf "coverage profile: %s\n" "$PROFILE"
printf "filtered profile: %s\n" "$FILTERED_PROFILE"
