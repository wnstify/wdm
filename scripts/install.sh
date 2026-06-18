#!/bin/sh
set -eu

repo="wnstify/wdm"
api_url="https://api.github.com/repos/${repo}/releases/latest"
issuer="https://token.actions.githubusercontent.com"
workflow_identity_base="https://github.com/${repo}/.github/workflows/release.yml@refs/tags"
cosign_version="v3.1.1"
cosign_asset="cosign-linux-amd64"
cosign_sha256="ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc"
binary_asset="wdm-linux-amd64"
catalog_asset="catalog-stable.tar.gz"
attestation_asset="attestation.json"
sbom_asset="wdm-linux-amd64.spdx.json"
checksums_asset="SHA256SUMS"
cosign_bundle_asset="SHA256SUMS.cosign.bundle"

die() {
	printf '%s\n' "wdm install: $*" >&2
	exit 1
}

need_cmd() {
	command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

need_any_install_method() {
	if command -v install >/dev/null 2>&1; then
		return 0
	fi

	need_cmd mkdir
	need_cmd cp
}

require_non_root() {
	if [ "$(id -u)" = "0" ]; then
		die "refusing to run as root"
	fi

	if [ -n "${SUDO_USER:-}" ]; then
		die "refusing to run under sudo"
	fi
}

require_platform() {
	os=$(uname -s)
	arch=$(uname -m)

	if [ "$os" != "Linux" ]; then
		die "unsupported operating system: $os"
	fi

	case "$arch" in
	x86_64 | amd64)
		;;
	*)
		die "unsupported architecture: $arch"
		;;
	esac
}

safe_install_dir() {
	case "$1" in
	"" | *[!ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._~/-]*)
		return 1
		;;
	/*)
		;;
	*)
		return 1
		;;
	esac

	case "$1" in
	*/../* | */.. | */.)
		return 1
		;;
	esac

	case "$1" in
	/ | /bin | /bin/* | /sbin | /sbin/* | /usr | /usr/* | /etc | /etc/* | /opt | /opt/* | /var | /var/* | /root | /root/*)
		return 1
		;;
	esac

	return 0
}

fetch_latest_release() {
	curl -fsSL \
		-H "Accept: application/vnd.github+json" \
		-H "X-GitHub-Api-Version: 2022-11-28" \
		-o "$tmpdir/release.json" \
		"$api_url" || die "failed to fetch latest release metadata"

	tag=""
	while IFS= read -r line; do
		case "$line" in
		*'"tag_name"'*)
			tag=${line#*\"tag_name\"}
			tag=${tag#*:}
			tag=${tag#*\"}
			tag=${tag%%\"*}
			break
			;;
		esac
	done <"$tmpdir/release.json"

	case "$tag" in
	v[0-9]*)
		;;
	*)
		die "latest release tag is missing or unsupported"
		;;
	esac

	case "$tag" in
	*[!ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-]*)
		die "latest release tag contains unsupported characters: $tag"
		;;
	esac
}

download_asset() {
	asset=$1
	url="https://github.com/${repo}/releases/download/${tag}/${asset}"

	curl -fsSL --proto '=https' --tlsv1.2 -o "$tmpdir/$asset" "$url" ||
		die "failed to download release asset: $asset"
}

bootstrap_cosign() {
	cosign_bin="$tmpdir/$cosign_asset"
	url="https://github.com/sigstore/cosign/releases/download/${cosign_version}/${cosign_asset}"

	curl -fsSL --proto '=https' --tlsv1.2 -o "$tmpdir/$cosign_asset" "$url" ||
		die "failed to download cosign verifier"

	(
		cd "$tmpdir"
		printf '%s  %s\n' "$cosign_sha256" "$cosign_asset" | sha256sum -c -
	) >/dev/null || die "cosign verifier checksum mismatch"

	chmod 0755 "$tmpdir/$cosign_asset" ||
		die "failed to mark cosign verifier executable"
}

require_checksum_entry() {
	expected=$1
	found=0

	while read -r checksum name rest || [ -n "$checksum$name$rest" ]; do
		[ -n "$name" ] || continue

		case "$name" in
		\**)
			name=${name#\*}
			;;
		esac

		if [ "$name" = "$expected" ]; then
			found=1
			break
		fi
	done <"$tmpdir/$checksums_asset"

	[ "$found" = "1" ] ||
		die "checksum manifest missing required asset: $expected"
}

verify_attestation() {
	asset=$1

	"$cosign_bin" verify-blob-attestation \
		--bundle "$tmpdir/$attestation_asset" \
		--type slsaprovenance1 \
		--certificate-identity "$identity" \
		--certificate-oidc-issuer "$issuer" \
		"$tmpdir/$asset" >/dev/null 2>&1 ||
		die "attestation verification failed for $asset"
}

verify_release() {
	identity="${workflow_identity_base}/${tag}"

	"$cosign_bin" verify-blob \
		--bundle "$tmpdir/$cosign_bundle_asset" \
		--certificate-identity "$identity" \
		--certificate-oidc-issuer "$issuer" \
		"$tmpdir/$checksums_asset" >/dev/null 2>&1 ||
		die "cosign verification failed"

	require_checksum_entry "$binary_asset"
	require_checksum_entry "$catalog_asset"
	require_checksum_entry "$attestation_asset"
	require_checksum_entry "$sbom_asset"

	(
		cd "$tmpdir"
		sha256sum -c "$checksums_asset"
	) >/dev/null || die "checksum verification failed"

	verify_attestation "$binary_asset"
	verify_attestation "$catalog_asset"
}

install_binary() {
	target="$install_dir/wdm"

	old_umask=$(umask)
	umask 022
	mkdir -p "$install_dir" || {
		umask "$old_umask"
		die "failed to create install directory: $install_dir"
	}
	umask "$old_umask"

	if command -v install >/dev/null 2>&1; then
		install -m 0755 "$tmpdir/$binary_asset" "$target" ||
			die "failed to install wdm to $target"
	else
		cp "$tmpdir/$binary_asset" "$target" ||
			die "failed to copy wdm to $target"
		chmod 0755 "$target" ||
			die "failed to mark wdm executable"
	fi
}

cleanup() {
	rm -rf "$tmpdir"
}

cleanup_signal() {
	cleanup
	exit 1
}

need_cmd curl
need_cmd sha256sum
need_cmd mktemp
need_cmd chmod
need_cmd mkdir
need_cmd rm
need_cmd id
need_cmd uname
need_any_install_method

require_non_root
require_platform

[ -n "${HOME:-}" ] || die "HOME is not set"
install_dir=${WDM_INSTALL_DIR:-"$HOME/.local/bin"}
safe_install_dir "$install_dir" ||
	die "WDM_INSTALL_DIR must be a user-local absolute path using only letters, numbers, '.', '_', '-', '~', and '/'"

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/wdm-install.XXXXXX") ||
	die "failed to create temporary directory"
trap cleanup EXIT
trap cleanup_signal HUP INT TERM

fetch_latest_release
bootstrap_cosign

download_asset "$binary_asset"
download_asset "$catalog_asset"
download_asset "$attestation_asset"
download_asset "$sbom_asset"
download_asset "$checksums_asset"
download_asset "$cosign_bundle_asset"

verify_release
install_binary

printf '%s\n' "wdm ${tag} installed to ${install_dir}/wdm"

case ":${PATH:-}:" in
*":$install_dir:"*)
	;;
*)
	printf '%s\n' "Add ${install_dir} to PATH, for example:"
	printf '%s\n' "  export PATH=\"${install_dir}:\$PATH\""
	;;
esac
