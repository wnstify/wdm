#!/bin/sh
# vm-verify: prove the current origin/dev tip on the remote rootless-Docker
# test server before a dev->main promotion. One SSH round trip resets to
# origin/dev, builds, seeds the dev catalog, runs the docker e2e suite, then a
# smoke matrix that installs each app and checks it is running, tears every
# managed stack down with one `wdm uninstall`, removes the matrix stack dirs,
# and fails closed if any wdm.managed container, network, or stack dir leaks.
# The server tees the whole run into ~/wdm-verify-logs/<UTC>-<sha>.log and ends
# with PASS/FAIL <sha>.
#
# `apps delete` is TTY-gated by design (PRD §19) and has no headless path, so
# teardown uses `wdm uninstall --yes --json`. Uninstall removes the running
# binary and wdm's share/state dirs but keeps the container-written stack dirs
# under ~/docker/<app>; those are subuid-owned, so `rootlesskit rm -rf` clears
# them where plain rm hits EACCES.
#
# Usage:
#   WDM_VM_SSH=user@testhost make vm-verify
#   WDM_VM_SSH=user@testhost sh scripts/ops/vm-verify.sh [--fresh] [--app id[,id...]]
#
# --fresh  reset wdm test state first: CLI uninstall (which wipes wdm's
#          share/state dirs itself), rootlesskit-clear the matrix stack dirs,
#          then wipe ~/.config/wdm.
# --app    override the default matrix (uptime-kuma,dockhand). Repeatable or
#          comma-separated.
set -eu

die() {
	printf '%s\n' "vm-verify: $*" >&2
	exit 1
}

usage() {
	printf '%s\n' "usage: WDM_VM_SSH=user@testhost sh scripts/ops/vm-verify.sh [--fresh] [--app id[,id...]]"
}

fresh=0
apps=""
while [ $# -gt 0 ]; do
	case "$1" in
	--fresh)
		fresh=1
		;;
	--app)
		shift
		[ $# -gt 0 ] || die "--app needs a value"
		apps="$apps $(printf '%s' "$1" | tr ',' ' ')"
		;;
	--app=*)
		apps="$apps $(printf '%s' "${1#--app=}" | tr ',' ' ')"
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		die "unknown argument: $1"
		;;
	esac
	shift
done

target=${WDM_VM_SSH:-}
[ -n "$target" ] || {
	usage >&2
	die "WDM_VM_SSH is not set (e.g. user@testhost)"
}

[ -n "$apps" ] || apps="uptime-kuma dockhand"
for app in $apps; do
	case "$app" in
	*[!a-z0-9-]* | "")
		die "invalid app id: $app"
		;;
	esac
done

# Word-split apps into separate ssh positional args; each is validated
# [a-z0-9-] above, so splitting is safe and intentional.
# shellcheck disable=SC2086
exec ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=15 "$target" sh -s -- "$fresh" $apps <<'REMOTE'
set -eu

die() {
	printf '%s\n' "vm-verify: $*" >&2
	exit 1
}

fresh=$1
shift
apps="$*"
[ -n "$apps" ] || die "no apps to verify"

repo="$HOME/wdm"
cd "$repo" || die "missing checkout: $repo"

ts=$(date -u +%Y%m%dT%H%M%SZ)
logdir="$HOME/wdm-verify-logs"
mkdir -p "$logdir"

# Fetch first so the log name carries the real dev sha; still log on failure.
if ! git fetch origin dev; then
	log="$logdir/$ts-fetchfail.log"
	printf 'git fetch origin dev failed\nFAIL unknown\n' | tee "$log" >&2
	exit 1
fi
sha=$(git rev-parse --short FETCH_HEAD)
log="$logdir/$ts-$sha.log"

verify() {
	if [ "$fresh" = "1" ]; then
		echo "== fresh: reset wdm test state =="
		if [ -x ./bin/wdm ]; then
			./bin/wdm uninstall --yes --json || die "fresh uninstall failed"
		fi
		# uninstall wipes wdm's own share/state dirs; clear the kept,
		# subuid-owned stack dirs and the config dir it leaves behind.
		for app in $apps; do
			rootlesskit rm -rf "$HOME/docker/$app" || die "fresh clear ~/docker/$app failed"
		done
		rm -rf "$HOME/.config/wdm"
	fi

	echo "== git reset --hard origin/dev ($sha) =="
	git reset --hard origin/dev || die "git reset failed"

	echo "== make build =="
	make build || die "make build failed"

	echo "== make dev-catalog-seed FORCE=1 =="
	make dev-catalog-seed FORCE=1 || die "dev-catalog-seed failed"

	# A leftover stack dir from a prior run makes install refuse; clear the
	# matrix dirs (subuid-owned) before installing.
	echo "== pre-install guard: clear matrix stack dirs =="
	for app in $apps; do
		[ ! -e "$HOME/docker/$app" ] || rootlesskit rm -rf "$HOME/docker/$app" || die "pre-install clear ~/docker/$app failed"
	done

	echo "== make e2e =="
	make e2e || die "make e2e failed"

	for app in $apps; do
		echo "== install $app =="
		./bin/wdm apps install "$app" --yes --json || die "install $app failed"
		running=$(docker ps -q --filter "label=wdm.app=$app" --filter status=running)
		[ -n "$running" ] || die "no running containers for $app"
	done

	# apps delete is TTY-gated (PRD §19); uninstall is the headless teardown.
	# It tears down every managed stack and removes the running binary itself,
	# which is fine here since nothing after this needs ./bin/wdm.
	echo "== uninstall (headless teardown of all managed stacks) =="
	./bin/wdm uninstall --yes --json || die "uninstall failed"

	echo "== remove matrix stack dirs =="
	for app in $apps; do
		rootlesskit rm -rf "$HOME/docker/$app" || die "remove ~/docker/$app failed"
	done

	echo "== leftover sweep =="
	leftover_c=$(docker ps -aq --filter "label=wdm.managed=true")
	[ -z "$leftover_c" ] || die "leftover managed containers: $leftover_c"
	leftover_n=$(docker network ls -q --filter "label=wdm.managed=true")
	[ -z "$leftover_n" ] || die "leftover managed networks: $leftover_n"
	for app in $apps; do
		[ ! -e "$HOME/docker/$app" ] || die "leftover stack dir: ~/docker/$app"
	done
}

# Tee live output to the server log while capturing verify's real exit status.
# verify runs bare (not in an if/&&/|| condition) so set -e stays armed inside
# it, and every critical step also fails closed with an explicit || die. The
# EXIT trap fires even when die/set -e aborts mid-pipeline, so FAIL <sha> is
# always teed to the log and the rc file is always written.
rc_file=$(mktemp)
{
	# shellcheck disable=SC2154 # st is assigned as the trap body's first statement
	trap 'st=$?; if [ "$st" -eq 0 ]; then echo "PASS $sha"; else echo "FAIL $sha"; fi; echo "$st" >"$rc_file"' EXIT
	verify
} 2>&1 | tee "$log"
rc=$(cat "$rc_file" 2>/dev/null || true)
rm -f "$rc_file"
[ -n "$rc" ] || rc=1
exit "$rc"
REMOTE
