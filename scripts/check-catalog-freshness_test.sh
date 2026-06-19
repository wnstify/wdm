#!/bin/sh
set -eu

script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd)
guard="$script_dir/check-catalog-freshness.sh"

failures=0
tmp_root=$(mktemp -d)
trap 'rm -r "$tmp_root"' EXIT HUP INT TERM

write_catalog() {
	repo=$1
	generated_at=$2

	cat >"$repo/catalog/stable/catalog.yaml" <<EOF
schema_version: 2
channel: stable
generated_at: "$generated_at"
apps: []
EOF
}

init_repo() {
	repo=$1

	mkdir -p "$repo/catalog/stable" "$repo/templates/app"
	(
		cd "$repo"
		# Use a branch name that is not a freshness-guard fallback candidate
		# (origin/dev, dev, origin/main, main), so the no-fallback case is
		# deterministic regardless of the runner's init.defaultBranch.
		git init -q -b freshness-test
		git config user.email test@example.invalid
		git config user.name "Test User"
	)
	write_catalog "$repo" "2026-05-21T00:00:00Z"
	printf '%s\n' "old template" >"$repo/templates/app/docker-compose.yml.tmpl"
	(
		cd "$repo"
		git add catalog/stable/catalog.yaml templates/app/docker-compose.yml.tmpl
		git commit -qm base
		git tag v1.0.0
	)
}

contains() {
	file=$1
	needle=$2

	grep -Fq "$needle" "$file"
}

run_guard() {
	repo=$1
	out=$2

	run_guard_with_ref "$repo" "$out" v1.0.0
}

run_guard_with_ref() {
	repo=$1
	out=$2
	ref=$3

	set +e
	(
		cd "$repo"
		WDM_CATALOG_BASE_REF="$ref" sh "$guard"
	) >"$out" 2>&1
	status=$?
	set -e

	return "$status"
}

expect_pass_with_ref() {
	name=$1
	repo=$2
	ref=$3
	want=$4
	out="$tmp_root/$name.out"

	if run_guard_with_ref "$repo" "$out" "$ref"; then
		if contains "$out" "$want"; then
			printf '%s\n' "ok - $name"
			return
		fi
		printf '%s\n' "not ok - $name: missing expected output: $want" >&2
	else
		printf '%s\n' "not ok - $name: guard failed" >&2
	fi

	sed -n '1,40p' "$out" >&2
	failures=$((failures + 1))
}

expect_fail_with_ref() {
	name=$1
	repo=$2
	ref=$3
	want=$4
	out="$tmp_root/$name.out"

	if run_guard_with_ref "$repo" "$out" "$ref"; then
		printf '%s\n' "not ok - $name: guard passed" >&2
		sed -n '1,40p' "$out" >&2
		failures=$((failures + 1))
		return
	fi
	if contains "$out" "$want"; then
		printf '%s\n' "ok - $name"
		return
	fi

	printf '%s\n' "not ok - $name: missing expected output: $want" >&2
	sed -n '1,40p' "$out" >&2
	failures=$((failures + 1))
}

expect_pass() {
	name=$1
	repo=$2
	want=$3
	out="$tmp_root/$name.out"

	if run_guard "$repo" "$out"; then
		if contains "$out" "$want"; then
			printf '%s\n' "ok - $name"
			return
		fi
		printf '%s\n' "not ok - $name: missing expected output: $want" >&2
	else
		printf '%s\n' "not ok - $name: guard failed" >&2
	fi

	sed -n '1,40p' "$out" >&2
	failures=$((failures + 1))
}

expect_fail() {
	name=$1
	repo=$2
	want=$3
	out="$tmp_root/$name.out"

	if run_guard "$repo" "$out"; then
		printf '%s\n' "not ok - $name: guard passed" >&2
		sed -n '1,40p' "$out" >&2
		failures=$((failures + 1))
		return
	fi
	if contains "$out" "$want"; then
		printf '%s\n' "ok - $name"
		return
	fi

	printf '%s\n' "not ok - $name: missing expected output: $want" >&2
	sed -n '1,40p' "$out" >&2
	failures=$((failures + 1))
}

repo="$tmp_root/no-change"
init_repo "$repo"
expect_pass "no catalog or template change passes" "$repo" "catalog freshness OK: no catalog or template changes"

repo="$tmp_root/template-unchanged-generated-at"
init_repo "$repo"
printf '%s\n' "new template" >"$repo/templates/app/docker-compose.yml.tmpl"
expect_fail "template change with unchanged generated_at fails" "$repo" \
	"catalog/templates changed without advancing generated_at"

repo="$tmp_root/catalog-only-unchanged-generated-at"
init_repo "$repo"
printf '%s\n' "# catalog-only freshness test" >>"$repo/catalog/stable/catalog.yaml"
expect_fail "catalog-only change with unchanged generated_at fails" "$repo" \
	"catalog/templates changed without advancing generated_at"

repo="$tmp_root/catalog-older-generated-at"
init_repo "$repo"
write_catalog "$repo" "2026-05-20T00:00:00Z"
expect_fail "catalog change with older generated_at fails" "$repo" \
	"head generated_at: 2026-05-20T00:00:00Z"

repo="$tmp_root/release-delta"
init_repo "$repo"
printf '%s\n' "new template" >"$repo/templates/app/docker-compose.yml.tmpl"
(
	cd "$repo"
	git add templates/app/docker-compose.yml.tmpl
	git commit -qm "change template without catalog version"
	printf '%s\n' "final release note" >CHANGELOG.md
	git add CHANGELOG.md
	git commit -qm "final release metadata"
)
expect_fail "release delta catches earlier template change" "$repo" \
	"templates/app/docker-compose.yml.tmpl"

repo="$tmp_root/template-advanced-generated-at"
init_repo "$repo"
printf '%s\n' "new template" >"$repo/templates/app/docker-compose.yml.tmpl"
write_catalog "$repo" "2026-06-18T16:00:00Z"
expect_pass "template change with advanced generated_at passes" "$repo" \
	"catalog freshness OK: 2026-05-21T00:00:00Z -> 2026-06-18T16:00:00Z"

bogus_ref=e60f3be7c6ad8a31cf7b783d982845b37773af1b

# Unreachable base ref (e.g. a force-push-orphaned tip) must fall back to a
# valid base instead of hard-failing. With the fallback base the freshness
# enforcement still runs: an advanced generated_at passes.
repo="$tmp_root/unreachable-base-advanced"
init_repo "$repo"
write_catalog "$repo" "2026-06-18T16:00:00Z"
(
	cd "$repo"
	git add catalog/stable/catalog.yaml
	git commit -qm "advance catalog generated_at"
)
expect_pass_with_ref "unreachable base ref falls back and passes" "$repo" "$bogus_ref" \
	"catalog freshness OK: 2026-05-21T00:00:00Z -> 2026-06-18T16:00:00Z"

# The fallback must not weaken enforcement: a template change against the
# fallback base without advancing generated_at still fails.
repo="$tmp_root/unreachable-base-not-advanced"
init_repo "$repo"
printf '%s\n' "new template" >"$repo/templates/app/docker-compose.yml.tmpl"
(
	cd "$repo"
	git add templates/app/docker-compose.yml.tmpl
	git commit -qm "change template without advancing generated_at"
)
expect_fail_with_ref "unreachable base ref still enforces freshness" "$repo" "$bogus_ref" \
	"catalog/templates changed without advancing generated_at"

# When no base ref can be established at all (single-commit checkout, no
# remote or fallback branch), skip gracefully instead of exiting non-zero.
repo="$tmp_root/unreachable-base-no-fallback"
init_repo "$repo"
expect_pass_with_ref "unreachable base ref with no fallback skips gracefully" "$repo" "$bogus_ref" \
	"catalog freshness OK: no base ref available to diff against (skipping)"

[ "$failures" -eq 0 ]
