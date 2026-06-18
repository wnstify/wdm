#!/bin/sh
# shellcheck disable=SC2016
set -eu

script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd)
install_script="$script_dir/install.sh"
shell_path=$(command -v sh)

failures=0

write_fake() {
	path=$1
	shift
	{
		printf '%s\n' '#!/bin/sh'
		printf '%s\n' 'set -eu'
		printf '%s\n' "$@"
	} >"$path"
	chmod 0755 "$path"
}

write_base_fakes() {
	bin_dir=$1

	write_fake "$bin_dir/id" \
		'if [ "${1:-}" = "-u" ]; then printf "%s\n" 1000; exit 0; fi' \
		'exit 1'

	write_fake "$bin_dir/uname" \
		'case "${1:-}" in' \
		'-s) printf "%s\n" Linux ;;' \
		'-m) printf "%s\n" x86_64 ;;' \
		'*) exit 1 ;;' \
		'esac'

	write_fake "$bin_dir/install" \
		'mode=' \
		'if [ "${1:-}" = "-m" ]; then mode=$2; shift 2; fi' \
		'[ "$#" -eq 2 ] || exit 1' \
		'cp "$1" "$2" || exit 1' \
		'[ -z "$mode" ] || chmod "$mode" "$2"'
}

write_sha256sum_fake() {
	bin_dir=$1

	write_fake "$bin_dir/sha256sum" \
		'[ "${1:-}" = "-c" ] || exit 1' \
		'if [ "${2:-}" = "-" ]; then' \
		'	while IFS= read -r _line; do :; done' \
		'	if [ "${WDM_INSTALL_TEST_SCENARIO:-}" = "bad_cosign_bootstrap_checksum" ]; then exit 1; fi' \
		'	exit 0' \
		'fi' \
		'[ -f "${2:-}" ] || exit 1' \
		'exit 0'
}

write_tar_fake() {
	bin_dir=$1

	write_fake "$bin_dir/tar" \
		'case "${1:-}" in' \
		'-tzf)' \
		'	case "${WDM_INSTALL_TEST_SCENARIO:-}" in' \
		'	unsafe_catalog_absolute_path)' \
		'		printf "%s\n" "/etc/passwd"' \
		'		;;' \
		'	unsafe_catalog_parent_path)' \
		'		printf "%s\n" "stable/../escape.yaml"' \
		'		;;' \
		'	unsafe_catalog_root)' \
		'		printf "%s\n" "other/file.txt"' \
		'		;;' \
		'	missing_catalog_manifest)' \
		'		printf "%s\n" "stable/" "templates/" "templates/uptime-kuma/docker-compose.yml.tmpl"' \
		'		;;' \
		'	missing_catalog_templates)' \
		'		printf "%s\n" "stable/" "stable/catalog.yaml"' \
		'		;;' \
		'	*)' \
		'		printf "%s\n" "stable/" "stable/catalog.yaml" "templates/" "templates/uptime-kuma/docker-compose.yml.tmpl"' \
		'		;;' \
		'	esac' \
		'	;;' \
		'-tvzf)' \
		'	case "${WDM_INSTALL_TEST_SCENARIO:-}" in' \
		'	unsafe_catalog_symlink_type)' \
		'		printf "%s\n" "drwxr-xr-x 0 root root 0 Jan 1 00:00 stable/"' \
		'		printf "%s\n" "-rw-r--r-- 0 root root 1 Jan 1 00:00 stable/catalog.yaml"' \
		'		printf "%s\n" "drwxr-xr-x 0 root root 0 Jan 1 00:00 templates/"' \
		'		printf "%s\n" "lrwxrwxrwx 0 root root 0 Jan 1 00:00 templates/uptime-kuma/docker-compose.yml.tmpl -> /etc/passwd"' \
		'		;;' \
		'	unsafe_catalog_hardlink_type)' \
		'		printf "%s\n" "drwxr-xr-x 0 root root 0 Jan 1 00:00 stable/"' \
		'		printf "%s\n" "hrw-r--r-- 0 root root 0 Jan 1 00:00 stable/catalog.yaml link to templates/source"' \
		'		printf "%s\n" "drwxr-xr-x 0 root root 0 Jan 1 00:00 templates/"' \
		'		printf "%s\n" "-rw-r--r-- 0 root root 1 Jan 1 00:00 templates/uptime-kuma/docker-compose.yml.tmpl"' \
		'		;;' \
		'	*)' \
		'		printf "%s\n" "drwxr-xr-x 0 root root 0 Jan 1 00:00 stable/"' \
		'		printf "%s\n" "-rw-r--r-- 0 root root 1 Jan 1 00:00 stable/catalog.yaml"' \
		'		printf "%s\n" "drwxr-xr-x 0 root root 0 Jan 1 00:00 templates/"' \
		'		printf "%s\n" "-rw-r--r-- 0 root root 1 Jan 1 00:00 templates/uptime-kuma/docker-compose.yml.tmpl"' \
		'		;;' \
		'	esac' \
		'	;;' \
		'-xzf)' \
		'	[ "${3:-}" = "-C" ] || exit 1' \
		'	dest=${4:-}' \
		'	[ -n "$dest" ] || exit 1' \
		'	mkdir -p "$dest/stable" "$dest/templates/uptime-kuma" || exit 1' \
		'	printf "%s\n" "schema_version: 1" >"$dest/stable/catalog.yaml" || exit 1' \
		'	printf "%s\n" "services: {}" >"$dest/templates/uptime-kuma/docker-compose.yml.tmpl" || exit 1' \
		'	;;' \
		'*)' \
		'	exit 1' \
		'	;;' \
		'esac'
}

write_mv_fake() {
	bin_dir=$1

	write_fake "$bin_dir/mv" \
		'[ -n "${WDM_INSTALL_TEST_REAL_MV:-}" ] || exit 1' \
		'if [ -n "${WDM_INSTALL_TEST_MV_LOG:-}" ]; then' \
		'	printf "%s -> %s\n" "${1:-}" "${2:-}" >>"$WDM_INSTALL_TEST_MV_LOG"' \
		'fi' \
		'if [ "${WDM_INSTALL_TEST_SCENARIO:-}" = "templates_promote_partial_failure" ]; then' \
		'	case "${1:-}:${2:-}" in' \
		'	*/.install.*/templates.new:*/catalogs/templates)' \
		'		mkdir -p "$2/uptime-kuma" || exit 1' \
		'		printf "%s\n" "partial-template" >"$2/uptime-kuma/docker-compose.yml.tmpl" || exit 1' \
		'		exit 1' \
		'		;;' \
		'	esac' \
		'fi' \
		'if [ "${WDM_INSTALL_TEST_SCENARIO:-}" = "manifest_promote_failure" ]; then' \
		'	case "${1:-}:${2:-}" in' \
		'	*/catalog.yaml.new:*/stable/catalog.yaml)' \
		'		exit 1' \
		'		;;' \
		'	esac' \
		'fi' \
		'exec "$WDM_INSTALL_TEST_REAL_MV" "$@"'
}

write_curl_fake() {
	bin_dir=$1

	write_fake "$bin_dir/curl" \
		'out=' \
		'want_out=0' \
		'for arg do' \
		'	if [ "$want_out" = "1" ]; then out=$arg; want_out=0; continue; fi' \
		'	if [ "$arg" = "-o" ]; then want_out=1; fi' \
		'done' \
		'[ -n "$out" ] || exit 1' \
		'case "$out" in' \
		'*/release.json)' \
		'	printf "%s\n" "{\"tag_name\":\"v1.2.3\"}" >"$out"' \
		'	;;' \
		'*/cosign-linux-amd64)' \
		'	{' \
		'		printf "%s\n" "#!/bin/sh"' \
		'		printf "%s\n" "set -eu"' \
		'		printf "%s\n" "cmd=\${1:-}"' \
		'		printf "%s\n" "last="' \
		'		printf "%s\n" "for arg do last=\$arg; done"' \
		'		printf "%s\n" "case \"\${WDM_INSTALL_TEST_SCENARIO:-}:\$cmd:\$last\" in"' \
		'		printf "%s\n" "bad_sha256sums_cosign:verify-blob:*) exit 1 ;;"' \
		'		printf "%s\n" "bad_binary_attestation:verify-blob-attestation:*wdm-linux-amd64) exit 1 ;;"' \
		'		printf "%s\n" "bad_catalog_attestation:verify-blob-attestation:*catalog-stable.tar.gz) exit 1 ;;"' \
		'		printf "%s\n" "esac"' \
		'		printf "%s\n" "exit 0"' \
		'	} >"$out"' \
		'	;;' \
		'*/SHA256SUMS)' \
		'	case "${WDM_INSTALL_TEST_SCENARIO:-}" in' \
		'	missing_catalog_checksum)' \
		'		printf "%s\n" "0000000000000000000000000000000000000000000000000000000000000000  wdm-linux-amd64" >"$out"' \
		'		printf "%s\n" "0000000000000000000000000000000000000000000000000000000000000000  attestation.json" >>"$out"' \
		'		printf "%s\n" "0000000000000000000000000000000000000000000000000000000000000000  wdm-linux-amd64.spdx.json" >>"$out"' \
		'		;;' \
		'	*)' \
		'		printf "%s\n" "0000000000000000000000000000000000000000000000000000000000000000  wdm-linux-amd64" >"$out"' \
		'		printf "%s\n" "0000000000000000000000000000000000000000000000000000000000000000  catalog-stable.tar.gz" >>"$out"' \
		'		printf "%s\n" "0000000000000000000000000000000000000000000000000000000000000000  attestation.json" >>"$out"' \
		'		printf "%s\n" "0000000000000000000000000000000000000000000000000000000000000000  wdm-linux-amd64.spdx.json" >>"$out"' \
		'		;;' \
		'	esac' \
		'	;;' \
		'*/catalog-stable.tar.gz)' \
		'	printf "%s\n" "catalog" >"$out"' \
		'	;;' \
		'*)' \
		'	printf "%s\n" "asset" >"$out"' \
		'	;;' \
		'esac'
}

contains() {
	file=$1
	needle=$2

	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in
		*"$needle"*)
			return 0
			;;
		esac
	done <"$file"

	return 1
}

run_case() {
	name=$1
	scenario=$2
	expected=$3

	tmpdir=$(mktemp -d "/tmp/wdm-install-test.XXXXXX") ||
		exit 1
	mkdir -p "$tmpdir/bin" "$tmpdir/home" "$tmpdir/tmp"

	if [ "$scenario" = "missing_sha256sum" ]; then
		write_curl_fake "$tmpdir/bin"
		test_path="$tmpdir/bin"
	elif [ "$scenario" = "missing_tar" ]; then
		write_base_fakes "$tmpdir/bin"
		write_sha256sum_fake "$tmpdir/bin"
		write_curl_fake "$tmpdir/bin"
		test_path="$tmpdir/bin"
	else
		write_base_fakes "$tmpdir/bin"
		write_sha256sum_fake "$tmpdir/bin"
		write_curl_fake "$tmpdir/bin"
		write_tar_fake "$tmpdir/bin"
		test_path="$tmpdir/bin:${PATH:-}"
	fi

	set +e
	HOME="$tmpdir/home" \
		PATH="$test_path" \
		TMPDIR="/tmp" \
		WDM_INSTALL_DIR="$tmpdir/home/.local/bin" \
		WDM_INSTALL_TEST_SCENARIO="$scenario" \
		"$shell_path" "$install_script" >"$tmpdir/stdout" 2>"$tmpdir/stderr"
	status=$?
	set -e

	if [ "$status" -eq 0 ]; then
		printf '%s\n' "not ok - $name: installer succeeded" >&2
		failures=$((failures + 1))
	elif ! contains "$tmpdir/stderr" "$expected"; then
		printf '%s\n' "not ok - $name: expected stderr to contain: $expected" >&2
		printf '%s\n' "stderr:" >&2
		sed -n '1,20p' "$tmpdir/stderr" >&2
		failures=$((failures + 1))
	else
		printf '%s\n' "ok - $name"
	fi

	rm -rf "$tmpdir"
}

run_private_install_dir_success_case() {
	name="existing private install dir keeps mode"
	scenario="success"
	tmpdir=$(mktemp -d "/tmp/wdm-install-test.XXXXXX") ||
		exit 1
	mkdir -p "$tmpdir/bin" "$tmpdir/home" "$tmpdir/tmp" "$tmpdir/home/.local/bin"
	chmod 0700 "$tmpdir/home/.local/bin"

	write_base_fakes "$tmpdir/bin"
	write_sha256sum_fake "$tmpdir/bin"
	write_curl_fake "$tmpdir/bin"
	write_tar_fake "$tmpdir/bin"
	test_path="$tmpdir/bin:${PATH:-}"

	set +e
	HOME="$tmpdir/home" \
		PATH="$test_path" \
		TMPDIR="/tmp" \
		WDM_INSTALL_DIR="$tmpdir/home/.local/bin" \
		WDM_INSTALL_TEST_SCENARIO="$scenario" \
		"$shell_path" "$install_script" >"$tmpdir/stdout" 2>"$tmpdir/stderr"
	status=$?
	set -e

	private_dir=$tmpdir/home/.local/bin
	catalog_dir=$tmpdir/home/.local/share/wdm/catalogs
	mode_match=$(find "$private_dir" -prune -type d -perm 700)
	if [ "$status" -ne 0 ]; then
		printf '%s\n' "not ok - $name: installer failed" >&2
		printf '%s\n' "stderr:" >&2
		sed -n '1,20p' "$tmpdir/stderr" >&2
		failures=$((failures + 1))
	elif [ "$mode_match" != "$private_dir" ]; then
		printf '%s\n' "not ok - $name: install dir mode is not 0700" >&2
		failures=$((failures + 1))
	elif [ ! -x "$private_dir/wdm" ]; then
		printf '%s\n' "not ok - $name: installed binary missing or not executable" >&2
		failures=$((failures + 1))
	elif [ ! -f "$catalog_dir/stable/catalog.yaml" ]; then
		printf '%s\n' "not ok - $name: seeded catalog manifest missing" >&2
		failures=$((failures + 1))
	elif [ ! -f "$catalog_dir/templates/uptime-kuma/docker-compose.yml.tmpl" ]; then
		printf '%s\n' "not ok - $name: seeded catalog template missing" >&2
		failures=$((failures + 1))
	else
		printf '%s\n' "ok - $name"
	fi

	rm -rf "$tmpdir"
}

run_xdg_data_home_success_case() {
	name="absolute XDG_DATA_HOME seeds catalog"
	scenario="success"
	tmpdir=$(mktemp -d "/tmp/wdm-install-test.XXXXXX") ||
		exit 1
	mkdir -p "$tmpdir/bin" "$tmpdir/home" "$tmpdir/data"

	write_base_fakes "$tmpdir/bin"
	write_sha256sum_fake "$tmpdir/bin"
	write_curl_fake "$tmpdir/bin"
	write_tar_fake "$tmpdir/bin"
	test_path="$tmpdir/bin:${PATH:-}"

	set +e
	HOME="$tmpdir/home" \
		XDG_DATA_HOME="$tmpdir/data" \
		PATH="$test_path" \
		TMPDIR="/tmp" \
		WDM_INSTALL_DIR="$tmpdir/home/.local/bin" \
		WDM_INSTALL_TEST_SCENARIO="$scenario" \
		"$shell_path" "$install_script" >"$tmpdir/stdout" 2>"$tmpdir/stderr"
	status=$?
	set -e

	catalog_dir=$tmpdir/data/wdm/catalogs
	fallback_catalog=$tmpdir/home/.local/share/wdm/catalogs/stable/catalog.yaml
	if [ "$status" -ne 0 ]; then
		printf '%s\n' "not ok - $name: installer failed" >&2
		printf '%s\n' "stderr:" >&2
		sed -n '1,20p' "$tmpdir/stderr" >&2
		failures=$((failures + 1))
	elif [ ! -f "$catalog_dir/stable/catalog.yaml" ]; then
		printf '%s\n' "not ok - $name: seeded XDG catalog manifest missing" >&2
		failures=$((failures + 1))
	elif [ -e "$fallback_catalog" ]; then
		printf '%s\n' "not ok - $name: fallback catalog path should not be used" >&2
		failures=$((failures + 1))
	else
		printf '%s\n' "ok - $name"
	fi

	rm -rf "$tmpdir"
}

run_relative_xdg_data_home_success_case() {
	name="relative XDG_DATA_HOME falls back to home data dir"
	scenario="success"
	tmpdir=$(mktemp -d "/tmp/wdm-install-test.XXXXXX") ||
		exit 1
	mkdir -p "$tmpdir/bin" "$tmpdir/home"

	write_base_fakes "$tmpdir/bin"
	write_sha256sum_fake "$tmpdir/bin"
	write_curl_fake "$tmpdir/bin"
	write_tar_fake "$tmpdir/bin"
	test_path="$tmpdir/bin:${PATH:-}"

	set +e
	HOME="$tmpdir/home" \
		XDG_DATA_HOME="relative-data" \
		PATH="$test_path" \
		TMPDIR="/tmp" \
		WDM_INSTALL_DIR="$tmpdir/home/.local/bin" \
		WDM_INSTALL_TEST_SCENARIO="$scenario" \
		"$shell_path" "$install_script" >"$tmpdir/stdout" 2>"$tmpdir/stderr"
	status=$?
	set -e

	catalog_manifest=$tmpdir/home/.local/share/wdm/catalogs/stable/catalog.yaml
	if [ "$status" -ne 0 ]; then
		printf '%s\n' "not ok - $name: installer failed" >&2
		printf '%s\n' "stderr:" >&2
		sed -n '1,20p' "$tmpdir/stderr" >&2
		failures=$((failures + 1))
	elif [ ! -f "$catalog_manifest" ]; then
		printf '%s\n' "not ok - $name: fallback catalog manifest missing" >&2
		failures=$((failures + 1))
	else
		printf '%s\n' "ok - $name"
	fi

	rm -rf "$tmpdir"
}

run_symlink_catalog_root_failure_case() {
	name="symlinked catalog root fails closed"
	scenario="success"
	tmpdir=$(mktemp -d "/tmp/wdm-install-test.XXXXXX") ||
		exit 1
	mkdir -p "$tmpdir/bin" "$tmpdir/home/.local/share/wdm" "$tmpdir/outside"
	ln -s "$tmpdir/outside" "$tmpdir/home/.local/share/wdm/catalogs"

	write_base_fakes "$tmpdir/bin"
	write_sha256sum_fake "$tmpdir/bin"
	write_curl_fake "$tmpdir/bin"
	write_tar_fake "$tmpdir/bin"
	test_path="$tmpdir/bin:${PATH:-}"

	set +e
	HOME="$tmpdir/home" \
		PATH="$test_path" \
		TMPDIR="/tmp" \
		WDM_INSTALL_DIR="$tmpdir/home/.local/bin" \
		WDM_INSTALL_TEST_SCENARIO="$scenario" \
		"$shell_path" "$install_script" >"$tmpdir/stdout" 2>"$tmpdir/stderr"
	status=$?
	set -e

	if [ "$status" -eq 0 ]; then
		printf '%s\n' "not ok - $name: installer succeeded" >&2
		failures=$((failures + 1))
	elif ! contains "$tmpdir/stderr" "refusing to seed catalog through a symlinked catalog directory"; then
		printf '%s\n' "not ok - $name: expected symlink refusal" >&2
		printf '%s\n' "stderr:" >&2
		sed -n '1,20p' "$tmpdir/stderr" >&2
		failures=$((failures + 1))
	else
		printf '%s\n' "ok - $name"
	fi

	rm -rf "$tmpdir"
}

run_existing_catalog_versions_success_case() {
	name="existing catalog versions are preserved"
	scenario="success"
	tmpdir=$(mktemp -d "/tmp/wdm-install-test.XXXXXX") ||
		exit 1
	mkdir -p "$tmpdir/bin" "$tmpdir/home/.local/share/wdm/catalogs/stable/.versions/v1.0.0"
	printf '%s\n' "snapshot" >"$tmpdir/home/.local/share/wdm/catalogs/stable/.versions/v1.0.0/provenance.txt"

	write_base_fakes "$tmpdir/bin"
	write_sha256sum_fake "$tmpdir/bin"
	write_curl_fake "$tmpdir/bin"
	write_tar_fake "$tmpdir/bin"
	test_path="$tmpdir/bin:${PATH:-}"

	set +e
	HOME="$tmpdir/home" \
		PATH="$test_path" \
		TMPDIR="/tmp" \
		WDM_INSTALL_DIR="$tmpdir/home/.local/bin" \
		WDM_INSTALL_TEST_SCENARIO="$scenario" \
		"$shell_path" "$install_script" >"$tmpdir/stdout" 2>"$tmpdir/stderr"
	status=$?
	set -e

	snapshot=$tmpdir/home/.local/share/wdm/catalogs/stable/.versions/v1.0.0/provenance.txt
	manifest=$tmpdir/home/.local/share/wdm/catalogs/stable/catalog.yaml
	if [ "$status" -ne 0 ]; then
		printf '%s\n' "not ok - $name: installer failed" >&2
		printf '%s\n' "stderr:" >&2
		sed -n '1,20p' "$tmpdir/stderr" >&2
		failures=$((failures + 1))
	elif [ ! -f "$manifest" ]; then
		printf '%s\n' "not ok - $name: active manifest missing" >&2
		failures=$((failures + 1))
	elif [ ! -f "$snapshot" ]; then
		printf '%s\n' "not ok - $name: version snapshot was removed" >&2
		failures=$((failures + 1))
	else
		printf '%s\n' "ok - $name"
	fi

	rm -rf "$tmpdir"
}

run_catalog_live_promotion_stays_in_catalog_dir_case() {
	name="catalog live promotion stays in catalog directory"
	scenario="success"
	tmpdir=$(mktemp -d "/tmp/wdm-install-test.XXXXXX") ||
		exit 1
	real_mv=$(command -v mv) || exit 1
	mkdir -p "$tmpdir/bin" "$tmpdir/home"

	write_base_fakes "$tmpdir/bin"
	write_sha256sum_fake "$tmpdir/bin"
	write_curl_fake "$tmpdir/bin"
	write_tar_fake "$tmpdir/bin"
	write_mv_fake "$tmpdir/bin"
	test_path="$tmpdir/bin:${PATH:-}"

	set +e
	HOME="$tmpdir/home" \
		PATH="$test_path" \
		TMPDIR="/tmp" \
		WDM_INSTALL_DIR="$tmpdir/home/.local/bin" \
		WDM_INSTALL_TEST_MV_LOG="$tmpdir/mv.log" \
		WDM_INSTALL_TEST_REAL_MV="$real_mv" \
		WDM_INSTALL_TEST_SCENARIO="$scenario" \
		"$shell_path" "$install_script" >"$tmpdir/stdout" 2>"$tmpdir/stderr"
	status=$?
	set -e

	stage_promoted=0
	tmp_promoted=0
	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in
		"$tmpdir/home/.local/share/wdm/catalogs/.install."*"/templates.new -> $tmpdir/home/.local/share/wdm/catalogs/templates")
			stage_promoted=1
			;;
		"/tmp/"*" -> $tmpdir/home/.local/share/wdm/catalogs/templates")
			tmp_promoted=1
			;;
		esac
	done <"$tmpdir/mv.log"

	if [ "$status" -ne 0 ]; then
		printf '%s\n' "not ok - $name: installer failed" >&2
		printf '%s\n' "stderr:" >&2
		sed -n '1,20p' "$tmpdir/stderr" >&2
		failures=$((failures + 1))
	elif [ "$stage_promoted" -ne 1 ]; then
		printf '%s\n' "not ok - $name: templates were not promoted from catalog staging" >&2
		failures=$((failures + 1))
	elif [ "$tmp_promoted" -ne 0 ]; then
		printf '%s\n' "not ok - $name: templates were promoted from /tmp" >&2
		failures=$((failures + 1))
	else
		printf '%s\n' "ok - $name"
	fi

	rm -rf "$tmpdir"
}

run_template_failure_rolls_back_case() {
	name="template promotion failure restores previous catalog"
	scenario="templates_promote_partial_failure"
	tmpdir=$(mktemp -d "/tmp/wdm-install-test.XXXXXX") ||
		exit 1
	real_mv=$(command -v mv) || exit 1
	mkdir -p "$tmpdir/bin" "$tmpdir/home/.local/share/wdm/catalogs/stable" "$tmpdir/home/.local/share/wdm/catalogs/templates/uptime-kuma"
	printf '%s\n' "old-manifest" >"$tmpdir/home/.local/share/wdm/catalogs/stable/catalog.yaml"
	printf '%s\n' "old-template" >"$tmpdir/home/.local/share/wdm/catalogs/templates/uptime-kuma/docker-compose.yml.tmpl"

	write_base_fakes "$tmpdir/bin"
	write_sha256sum_fake "$tmpdir/bin"
	write_curl_fake "$tmpdir/bin"
	write_tar_fake "$tmpdir/bin"
	write_mv_fake "$tmpdir/bin"
	test_path="$tmpdir/bin:${PATH:-}"

	set +e
	HOME="$tmpdir/home" \
		PATH="$test_path" \
		TMPDIR="/tmp" \
		WDM_INSTALL_DIR="$tmpdir/home/.local/bin" \
		WDM_INSTALL_TEST_REAL_MV="$real_mv" \
		WDM_INSTALL_TEST_SCENARIO="$scenario" \
		"$shell_path" "$install_script" >"$tmpdir/stdout" 2>"$tmpdir/stderr"
	status=$?
	set -e

	manifest=$tmpdir/home/.local/share/wdm/catalogs/stable/catalog.yaml
	template=$tmpdir/home/.local/share/wdm/catalogs/templates/uptime-kuma/docker-compose.yml.tmpl
	if [ "$status" -eq 0 ]; then
		printf '%s\n' "not ok - $name: installer succeeded" >&2
		failures=$((failures + 1))
	elif ! contains "$tmpdir/stderr" "failed to install catalog templates"; then
		printf '%s\n' "not ok - $name: expected template failure" >&2
		printf '%s\n' "stderr:" >&2
		sed -n '1,20p' "$tmpdir/stderr" >&2
		failures=$((failures + 1))
	elif ! contains "$manifest" "old-manifest"; then
		printf '%s\n' "not ok - $name: previous manifest was not preserved" >&2
		failures=$((failures + 1))
	elif ! contains "$template" "old-template"; then
		printf '%s\n' "not ok - $name: previous templates were not restored" >&2
		failures=$((failures + 1))
	else
		printf '%s\n' "ok - $name"
	fi

	rm -rf "$tmpdir"
}

run_manifest_failure_rolls_back_case() {
	name="manifest promotion failure restores previous catalog"
	scenario="manifest_promote_failure"
	tmpdir=$(mktemp -d "/tmp/wdm-install-test.XXXXXX") ||
		exit 1
	real_mv=$(command -v mv) || exit 1
	mkdir -p "$tmpdir/bin" "$tmpdir/home/.local/share/wdm/catalogs/stable" "$tmpdir/home/.local/share/wdm/catalogs/templates/uptime-kuma"
	printf '%s\n' "old-manifest" >"$tmpdir/home/.local/share/wdm/catalogs/stable/catalog.yaml"
	printf '%s\n' "old-template" >"$tmpdir/home/.local/share/wdm/catalogs/templates/uptime-kuma/docker-compose.yml.tmpl"

	write_base_fakes "$tmpdir/bin"
	write_sha256sum_fake "$tmpdir/bin"
	write_curl_fake "$tmpdir/bin"
	write_tar_fake "$tmpdir/bin"
	write_mv_fake "$tmpdir/bin"
	test_path="$tmpdir/bin:${PATH:-}"

	set +e
	HOME="$tmpdir/home" \
		PATH="$test_path" \
		TMPDIR="/tmp" \
		WDM_INSTALL_DIR="$tmpdir/home/.local/bin" \
		WDM_INSTALL_TEST_REAL_MV="$real_mv" \
		WDM_INSTALL_TEST_SCENARIO="$scenario" \
		"$shell_path" "$install_script" >"$tmpdir/stdout" 2>"$tmpdir/stderr"
	status=$?
	set -e

	manifest=$tmpdir/home/.local/share/wdm/catalogs/stable/catalog.yaml
	template=$tmpdir/home/.local/share/wdm/catalogs/templates/uptime-kuma/docker-compose.yml.tmpl
	if [ "$status" -eq 0 ]; then
		printf '%s\n' "not ok - $name: installer succeeded" >&2
		failures=$((failures + 1))
	elif ! contains "$tmpdir/stderr" "failed to install stable catalog"; then
		printf '%s\n' "not ok - $name: expected manifest failure" >&2
		printf '%s\n' "stderr:" >&2
		sed -n '1,20p' "$tmpdir/stderr" >&2
		failures=$((failures + 1))
	elif ! contains "$manifest" "old-manifest"; then
		printf '%s\n' "not ok - $name: previous manifest was not restored" >&2
		failures=$((failures + 1))
	elif ! contains "$template" "old-template"; then
		printf '%s\n' "not ok - $name: previous templates were not restored" >&2
		failures=$((failures + 1))
	else
		printf '%s\n' "ok - $name"
	fi

	rm -rf "$tmpdir"
}

run_private_install_dir_success_case
run_xdg_data_home_success_case
run_relative_xdg_data_home_success_case
run_symlink_catalog_root_failure_case
run_existing_catalog_versions_success_case
run_catalog_live_promotion_stays_in_catalog_dir_case
run_template_failure_rolls_back_case
run_manifest_failure_rolls_back_case
run_case "missing checksum entry fails closed" \
	"missing_catalog_checksum" \
	"checksum manifest missing required asset: catalog-stable.tar.gz"
run_case "missing sha256sum fails closed" \
	"missing_sha256sum" \
	"missing required command: sha256sum"
run_case "missing tar fails closed" \
	"missing_tar" \
	"missing required command: tar"
run_case "bad cosign bootstrap checksum fails closed" \
	"bad_cosign_bootstrap_checksum" \
	"cosign verifier checksum mismatch"
run_case "bad SHA256SUMS cosign verification fails closed" \
	"bad_sha256sums_cosign" \
	"cosign verification failed"
run_case "binary attestation failure fails closed" \
	"bad_binary_attestation" \
	"attestation verification failed for wdm-linux-amd64"
run_case "catalog attestation failure fails closed" \
	"bad_catalog_attestation" \
	"attestation verification failed for catalog-stable.tar.gz"
run_case "catalog bundle missing manifest fails closed" \
	"missing_catalog_manifest" \
	"catalog bundle missing required file: stable/catalog.yaml"
run_case "catalog bundle missing templates fails closed" \
	"missing_catalog_templates" \
	"catalog bundle missing required directory: templates"
run_case "absolute catalog bundle path fails closed" \
	"unsafe_catalog_absolute_path" \
	"catalog bundle contains unsafe path: /etc/passwd"
run_case "parent catalog bundle path fails closed" \
	"unsafe_catalog_parent_path" \
	"catalog bundle contains unsafe path: stable/../escape.yaml"
run_case "unsupported catalog bundle root fails closed" \
	"unsafe_catalog_root" \
	"catalog bundle contains unsupported path: other/file.txt"
run_case "catalog symlink member type fails closed" \
	"unsafe_catalog_symlink_type" \
	"catalog bundle contains unsupported entry type"
run_case "catalog hardlink member type fails closed" \
	"unsafe_catalog_hardlink_type" \
	"catalog bundle contains unsupported entry type"

[ "$failures" -eq 0 ]
