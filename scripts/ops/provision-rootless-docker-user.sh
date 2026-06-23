#!/usr/bin/env bash
set -euo pipefail

USERNAME=""
DRY_RUN=0
SKIP_HOST_CHECK=0
DOCKER_VERSION="29.6.0"
DOCKER_ARCH="x86_64"
DOCKER_STATIC_BASE_URL="https://download.docker.com/linux/static/stable"
DOCKER_TARBALL_SHA256="4d2f6782406b56eb43a519ad5078a6a79abe4d663328acb69136aceff5e05224"
DOCKER_ROOTLESS_TARBALL_SHA256="449460e9419253a438bc16d28c86736380f20171dfbf52c0e0af5ab597d3e06b"
DOCKER_COMPOSE_VERSION="v5.1.2"
DOCKER_COMPOSE_SHORT_VERSION="5.1.2"
DOCKER_COMPOSE_BASE_URL="https://github.com/docker/compose/releases/download"
DOCKER_COMPOSE_SHA256="c372e512a36e67716b0b3a1264ccdc461dec7a7beff601b81f7c5fb008e3511e"

usage() {
  cat <<'USAGE'
Usage: scripts/ops/provision-rootless-docker-user.sh [--user USERNAME] [--dry-run] [--skip-host-check]

Creates a dedicated Linux user, installs Docker rootless for that user, starts
the user-scoped Docker service, and verifies with docker run --rm hello-world.

Run as root or through sudo on the target server.

Options:
  --user USERNAME      User to create/configure. Prompts when omitted.
  --dry-run            Print commands without changing the system.
  --skip-host-check    Skip the unprivileged proc-mount capability precheck.
  -h, --help           Show this help.

Pinned artifacts:
  Docker static release: 29.6.0, x86_64, stable channel.
  Docker Compose plugin: v5.1.2, x86_64.
USAGE
}

fail() {
  printf 'provision-rootless-docker-user: %s\n' "$*" >&2
  exit 1
}

run() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '+ %s\n' "$*"
    return
  fi

  "$@"
}

run_shell() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '+ %s\n' "$*"
    return
  fi

  sh -c "$*"
}

validate_username() {
  local value="$1"

  case "$value" in
    root|daemon|bin|sys|sync|games|man|lp|mail|news|uucp|proxy|www-data|backup|list|irc|_apt|nobody)
      fail "Refusing unsafe username: $value"
      ;;
    ''|.*|-*|*'..'*|*[^a-zA-Z0-9_-]*)
      fail "Invalid username: $value"
      ;;
  esac

  [ "${#value}" -le 32 ] || fail "Invalid username: $value"
}

prompt_username() {
  local value

  printf 'Rootless Docker username: ' >&2
  IFS= read -r value
  USERNAME="$value"
}

user_exists() {
  getent passwd "$USERNAME" >/dev/null 2>&1
}

ensure_not_in_docker_group() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '+ id -nG %s | grep -qw docker && fail\n' "$USERNAME"
    return
  fi

  if id -nG "$USERNAME" | grep -qw docker; then
    fail "User $USERNAME is in the docker group. Remove that membership before provisioning rootless Docker."
  fi
}

ensure_root() {
  [ "$(id -u)" -eq 0 ] || fail "Run as root or with sudo."
}

install_prerequisites() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '+ apt-get update\n'
    printf '+ apt-get install -y ca-certificates curl dbus-user-session iproute2 procps sudo tar uidmap\n'
    return
  fi

  if ! command -v apt-get >/dev/null 2>&1; then
    printf 'apt-get not found; skipping package prerequisite installation\n'
    return
  fi

  run apt-get update
  run apt-get install -y ca-certificates curl dbus-user-session iproute2 procps sudo tar uidmap
}

ensure_ipv4_forwarding() {
  local config_file="/etc/sysctl.d/99-webnestify-rootless-docker.conf"

  run_shell "printf '%s\n' 'net.ipv4.ip_forward=1' > '$config_file'"
  run sysctl --system
}

next_subid_start() {
  local file="$1"

  awk -F: '
    BEGIN { max = 231072 }
    $2 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/ {
      end = $2 + $3
      if (end > max) {
        max = end
      }
    }
    END { print max }
  ' "$file"
}

ensure_subid() {
  local file="$1"
  local start

  if grep -q "^${USERNAME}:" "$file" 2>/dev/null; then
    return
  fi

  if [ "$DRY_RUN" -eq 1 ]; then
    start=231072
  else
    start="$(next_subid_start "$file")"
  fi

  run_shell "printf '%s:%s:65536\n' '$USERNAME' '$start' >> '$file'"
}

append_profile_block() {
  local profile_file="$1"

  run touch "$profile_file"
  if [ "$DRY_RUN" -eq 0 ] && grep -q "# Webnestify rootless Docker" "$profile_file"; then
    return
  fi

  run_shell "cat >> '$profile_file' <<'EOF'

# Webnestify rootless Docker
export PATH=\"\$HOME/bin:\$PATH\"
if [ -z \"\${XDG_RUNTIME_DIR:-}\" ]; then
  export XDG_RUNTIME_DIR=\"/run/user/\$(id -u)\"
fi
export DOCKER_HOST=\"unix://\${XDG_RUNTIME_DIR}/docker.sock\"
EOF"

  run chown "$USERNAME:$USERNAME" "$profile_file"
  run chmod 0644 "$profile_file"
}

run_as_user() {
  local command="$1"

  run sudo -H -u "$USERNAME" bash -lc "$command"
}

ensure_user_manager() {
  local uid

  if [ "$DRY_RUN" -eq 1 ]; then
    printf '+ systemctl start user@<uid>.service\n'
    return
  fi

  uid="$(id -u "$USERNAME")"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl start "user@${uid}.service" >/dev/null 2>&1 || true
  fi
}

ensure_user_supports_rootless_proc() {
  local command

  # shellcheck disable=SC2016
  command='set -euo pipefail
command -v unshare >/dev/null 2>&1
tmpdir="$(mktemp -d)"
trap '\''rm -rf "$tmpdir"'\'' EXIT
mkdir -p "$tmpdir/proc"
unshare -Urmpf sh -c "mount -t proc proc \"$tmpdir/proc\" && umount \"$tmpdir/proc\""'

  if [ "$DRY_RUN" -eq 1 ]; then
    printf '+ sudo -H -u %s bash -lc rootless-proc-mount-precheck\n' "$USERNAME"
    return
  fi

  if ! sudo -H -u "$USERNAME" bash -lc "$command" >/dev/null 2>&1; then
    fail "This host blocks rootless Docker private proc mounts for $USERNAME. Use another VM/server or resolve host user namespace policy first."
  fi
}

ensure_supported_docker_artifact_target() {
  local machine_arch

  machine_arch="$(uname -m)"
  [ "$machine_arch" = "$DOCKER_ARCH" ] || fail "Pinned Docker artifacts support $DOCKER_ARCH only; target reports $machine_arch."
}

install_rootless_docker() {
  local command
  local docker_url
  local rootless_url
  local compose_url

  docker_url="${DOCKER_STATIC_BASE_URL}/${DOCKER_ARCH}/docker-${DOCKER_VERSION}.tgz"
  rootless_url="${DOCKER_STATIC_BASE_URL}/${DOCKER_ARCH}/docker-rootless-extras-${DOCKER_VERSION}.tgz"
  compose_url="${DOCKER_COMPOSE_BASE_URL}/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-${DOCKER_ARCH}"

  command="set -euo pipefail
uid=\"\$(id -u)\"
export PATH=\"\$HOME/bin:\$PATH\"
export XDG_RUNTIME_DIR=\"/run/user/\$uid\"
export DBUS_SESSION_BUS_ADDRESS=\"unix:path=\${XDG_RUNTIME_DIR}/bus\"
tmpdir=\"\$(mktemp -d)\"
trap 'rm -rf \"\$tmpdir\"' EXIT
export PATH=\"\$HOME/bin:\$PATH\"
compose_plugin=\"\$HOME/.docker/cli-plugins/docker-compose\"
if [ -x \"\$HOME/bin/docker\" ] && [ -x \"\$HOME/bin/dockerd-rootless-setuptool.sh\" ]; then
  installed=\"\$(\"\$HOME/bin/docker\" --version 2>/dev/null || true)\"
  case \"\$installed\" in
    *\"version $DOCKER_VERSION,\"*) echo \"# Existing Docker $DOCKER_VERSION detected\" ;;
    *) echo \"Existing Docker installation does not match pinned version $DOCKER_VERSION\" >&2; exit 1 ;;
  esac
else
  curl -fsSL '$docker_url' -o \"\$tmpdir/docker.tgz\"
  printf '%s  %s\n' '$DOCKER_TARBALL_SHA256' \"\$tmpdir/docker.tgz\" | sha256sum -c -
  curl -fsSL '$rootless_url' -o \"\$tmpdir/rootless.tgz\"
  printf '%s  %s\n' '$DOCKER_ROOTLESS_TARBALL_SHA256' \"\$tmpdir/rootless.tgz\" | sha256sum -c -
  mkdir -p \"\$HOME/bin\"
  tar zxf \"\$tmpdir/docker.tgz\" -C \"\$HOME/bin\" --strip-components=1
  tar zxf \"\$tmpdir/rootless.tgz\" -C \"\$HOME/bin\" --strip-components=1
fi
if [ -x \"\$compose_plugin\" ]; then
  installed_compose=\"\$(\"\$compose_plugin\" version --short 2>/dev/null || true)\"
  if [ \"\$installed_compose\" = \"$DOCKER_COMPOSE_SHORT_VERSION\" ]; then
    echo \"# Existing Docker Compose $DOCKER_COMPOSE_VERSION detected\"
  else
    echo \"Existing Docker Compose plugin does not match pinned version $DOCKER_COMPOSE_VERSION\" >&2
    exit 1
  fi
else
  curl -fsSL '$compose_url' -o \"\$tmpdir/docker-compose\"
  printf '%s  %s\n' '$DOCKER_COMPOSE_SHA256' \"\$tmpdir/docker-compose\" | sha256sum -c -
  mkdir -p \"\$(dirname \"\$compose_plugin\")\"
  cp \"\$tmpdir/docker-compose\" \"\$compose_plugin\"
  chmod 0755 \"\$compose_plugin\"
fi
if [ ! -f \"\$HOME/.config/systemd/user/docker.service\" ]; then
  dockerd-rootless-setuptool.sh install
fi
systemctl --user daemon-reload
systemctl --user enable --now docker.service
docker context use rootless
docker context update rootless --docker \"host=unix://\${XDG_RUNTIME_DIR}/docker.sock\"
docker compose version
docker ps
docker run --rm hello-world"

  run_as_user "$command"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --user)
      [ "$#" -ge 2 ] || fail "--user requires a value."
      USERNAME="$2"
      shift 2
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --skip-host-check)
      SKIP_HOST_CHECK=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      fail "Unknown argument: $1"
      ;;
  esac
done

if [ -z "$USERNAME" ]; then
  prompt_username
fi

validate_username "$USERNAME"

if [ "$DRY_RUN" -eq 0 ]; then
  ensure_root
  ensure_supported_docker_artifact_target
fi

install_prerequisites
ensure_ipv4_forwarding

if user_exists; then
  printf 'User already exists: %s\n' "$USERNAME"
else
  run useradd --create-home --shell /bin/bash "$USERNAME"
fi

ensure_not_in_docker_group

if [ "$DRY_RUN" -eq 1 ]; then
  home_dir="/home/$USERNAME"
else
  home_dir="$(getent passwd "$USERNAME" | cut -d: -f6)"
  [ -n "$home_dir" ] || fail "Could not resolve home directory for user: $USERNAME"
fi

ensure_subid /etc/subuid
ensure_subid /etc/subgid

if [ "$SKIP_HOST_CHECK" -eq 0 ]; then
  ensure_user_supports_rootless_proc
fi

run loginctl enable-linger "$USERNAME"
ensure_user_manager
append_profile_block "$home_dir/.profile"
append_profile_block "$home_dir/.bashrc"
install_rootless_docker

printf 'Rootless Docker is ready for user %s\n' "$USERNAME"
