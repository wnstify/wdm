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

	if [ "$scenario" != "missing_sha256sum" ]; then
		write_base_fakes "$tmpdir/bin"
		write_sha256sum_fake "$tmpdir/bin"
		write_curl_fake "$tmpdir/bin"
		test_path="$tmpdir/bin:${PATH:-}"
	else
		write_curl_fake "$tmpdir/bin"
		test_path="$tmpdir/bin"
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
	else
		printf '%s\n' "ok - $name"
	fi

	rm -rf "$tmpdir"
}

run_private_install_dir_success_case
run_case "missing checksum entry fails closed" \
	"missing_catalog_checksum" \
	"checksum manifest missing required asset: catalog-stable.tar.gz"
run_case "missing sha256sum fails closed" \
	"missing_sha256sum" \
	"missing required command: sha256sum"
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

[ "$failures" -eq 0 ]
