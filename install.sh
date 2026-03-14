#!/usr/bin/env bash

if [[ -r "${0:-}" ]] && grep -q $'\r' "$0" 2>/dev/null; then
  tmp="$(mktemp)"
  tr -d '\r' < "$0" > "$tmp"
  chmod +x "$tmp"
  exec /usr/bin/env bash "$tmp" "$@"
fi

if [[ -z "${BASH_VERSION:-}" ]]; then
  echo "Please run with bash."
  echo "Example:"
  echo "  curl -fsSL 'https://bash.ad-panel.com/install.sh?nocache=$(date +%s)' | tr -d '\r' | sudo bash -s -- --yes"
  exit 1
fi

set -eEu
IFS=$'\n\t'
set -o pipefail

REPO_URL="https://github.com/antonndev/ADPanel-Daemon.git"
NODE_DIR="/var/lib/node"
GO_DIR="/var/lib/gonode"
BIN_DST="/usr/local/bin/adnode"
GO_VERSION="1.24.11"
GO_INSTALL_ROOT="/usr/local/go"
GO_BIN_LINK="/usr/local/bin/go"
GOFMT_BIN_LINK="/usr/local/bin/gofmt"

SERVICE_NAME="ADaemon"
UNIT_NAME="${SERVICE_NAME}.service"
UNIT_PATH="/etc/systemd/system/${UNIT_NAME}"
LEGACY_SERVICE_NAME="adpanel-daemon"
LEGACY_UNIT_NAME="${LEGACY_SERVICE_NAME}.service"
LEGACY_UNIT_PATH="/etc/systemd/system/${LEGACY_UNIT_NAME}"
LOG_FILE="/var/log/adpanel-daemon-install.log"

STATE_DIR="/var/lib/adpanel-installer"
STATE_FILE="${STATE_DIR}/state.env"

ASSUME_YES=0
for arg in "$@"; do
  case "$arg" in
    --yes|-y) ASSUME_YES=1 ;;
  esac
done

TTY_OK=0
if [[ -r /dev/tty ]]; then
  exec 3</dev/tty
  TTY_OK=1
fi

if [[ ! -t 0 ]]; then
  exec </dev/null
fi

UI=0
if [[ -t 1 ]] && command -v tput >/dev/null 2>&1; then UI=1; fi

if (( UI )); then
  C_RESET=$'\033[0m'
  C_BOLD=$'\033[1m'
  C_DIM=$'\033[2m'
  C_WHITE=$'\033[97m'
  C_CYAN=$'\033[96m'
  C_GREEN=$'\033[92m'
  C_YELLOW=$'\033[93m'
  C_RED=$'\033[91m'
else
  C_RESET=""; C_BOLD=""; C_DIM=""; C_WHITE=""; C_CYAN=""; C_GREEN=""; C_YELLOW=""; C_RED=""
fi

t_cols() { (( UI )) && tput cols || echo 80; }
cup() { (( UI )) && printf '\033[%s;%sH' "$1" "$2"; }
clr() { (( UI )) && printf '\033[2J\033[H'; }
clreol() { (( UI )) && printf '\033[2K'; }
hide_cursor() { (( UI )) && printf '\033[?25l'; }
show_cursor() { (( UI )) && printf '\033[?25h'; }

W=80
ROW_TITLE=2
ROW_RULE=3
ROW_ASCII=5
ROW_DOMAIN=12
ROW_STATUS=14
ROW_PROMPT=16
ROW_INPUT=17

BAR_HEIGHT=12
BAR_ROW=4
BAR_COL=78
BAR_PCT_COL=72

center_at_plain() {
  local row="$1"; local plain="$2"; local pre="${3:-}"; local suf="${4:-}"
  local w="$W"
  local len="${#plain}"
  local col=$(( (w - len) / 2 + 1 ))
  (( col < 1 )) && col=1
  cup "$row" "$col"
  printf "%s%s%s" "$pre" "$plain" "$suf"
}

line_rule() {
  local row="$1"
  local s="────────────────────────────────────────────────────────"
  center_at_plain "$row" "$s" "${C_DIM}" "${C_RESET}"
}

draw_ascii_adaemon() {
  local row="$ROW_ASCII"
  local line
  while IFS= read -r line; do
    center_at_plain "$row" "$line" "${C_CYAN}${C_BOLD}" "${C_RESET}"
    row=$((row+1))
  done <<'ASCII'
    _    ____                                 
   / \  |  _ \  __ _  ___ _ __ ___   ___  _ __ 
  / _ \ | | | |/ _` |/ _ \ '_ ` _ \ / _ \| '_ \
 / ___ \| |_| | (_| |  __/ | | | | | (_) | | | |
/_/   \_\____/ \__,_|\___|_| |_| |_|\___/|_| |_|
ASCII
}

status_set() {
  local msg_plain="$1"
  if (( UI )); then
    cup "$ROW_STATUS" 1; clreol
    center_at_plain "$ROW_STATUS" "$msg_plain" "${C_WHITE}${C_BOLD}" "${C_RESET}"
  else
    echo "==> ${msg_plain}"
  fi
}

prompt_clear() {
  (( UI )) || return 0
  cup "$ROW_PROMPT" 1; clreol
  cup "$ROW_INPUT" 1; clreol
}

prompt_set() {
  local p_plain="$1"
  if (( UI )); then
    prompt_clear
    center_at_plain "$ROW_PROMPT" "$p_plain" "${C_WHITE}${C_BOLD}" "${C_RESET}"
    center_at_plain "$ROW_INPUT" "› " "${C_CYAN}${C_BOLD}" "${C_RESET}"
    local col=$(( (W - 2) / 2 + 3 ))
    cup "$ROW_INPUT" "$col"
  else
    echo
    echo "${p_plain} [yes/no]"
    printf "> "
  fi
}

ui_splash() {
  W="$(t_cols)"
  BAR_COL=$(( W - 2 ))
  (( BAR_COL < 10 )) && BAR_COL=10
  BAR_PCT_COL=$(( BAR_COL - 7 ))
  (( BAR_PCT_COL < 1 )) && BAR_PCT_COL=1

  clr
  hide_cursor

  center_at_plain "$ROW_TITLE" "ADPanel Daemon Installation" "${C_WHITE}${C_BOLD}" "${C_RESET}"
  line_rule "$ROW_RULE"
  draw_ascii_adaemon
  center_at_plain "$ROW_DOMAIN" "ad-panel.com" "${C_DIM}" "${C_RESET}"

  if (( UI )); then
    for ((i=0;i<BAR_HEIGHT;i++)); do
      cup "$((BAR_ROW+i))" "$BAR_PCT_COL"; printf "%s" "       "
      cup "$((BAR_ROW+i))" "$BAR_COL"; printf "%s" " "
    done
  fi
}

bar_render() {
  local percent="$1"
  local tick="$2"
  (( UI )) || return 0

  (( percent < 0 )) && percent=0
  (( percent > 100 )) && percent=100

  local filled=$(( percent * BAR_HEIGHT / 100 ))
  local pulse_on=$(( tick % 2 ))

  local mid=$(( BAR_ROW + BAR_HEIGHT / 2 ))
  cup "$mid" "$BAR_PCT_COL"
  printf "%5s " "${percent}%"

  for ((i=0;i<BAR_HEIGHT;i++)); do
    local level_from_bottom=$(( BAR_HEIGHT - i ))
    local ch="░"
    if (( level_from_bottom <= filled )); then
      ch="█"
    else
      if (( percent < 100 )) && (( level_from_bottom == filled + 1 )); then
        if (( pulse_on )); then ch="▓"; else ch="▒"; fi
      fi
    fi
    cup "$((BAR_ROW+i))" "$BAR_COL"
    printf "%s" "${C_WHITE}${ch}${C_RESET}"
  done
}

ui_bye() {
  if (( UI )); then
    clr
    center_at_plain 4 "Opsss, we have to go... bye bye....!" "${C_YELLOW}${C_BOLD}" "${C_RESET}"
    cup 6 1
  else
    echo "Opsss, we have to go... bye bye....!"
  fi
}

cleanup() {
  show_cursor
  if (( UI )); then
    cup "$((BAR_ROW+BAR_HEIGHT+3))" 1
    printf "\n"
  fi
}
trap cleanup EXIT

fail() {
  local rc="${1:-1}"
  show_cursor
  echo
  echo "${C_RED}${C_BOLD}✗ Operation failed.${C_RESET}"
  echo "${C_DIM}See: ${LOG_FILE}${C_RESET}"
  echo
  tail -n 220 "${LOG_FILE}" 2>/dev/null || true
  exit "$rc"
}

trap 'status_set "Installer is running… please wait."; ' INT
trap 'status_set "Stop requested — ignored. Installer continues…"; ' TERM QUIT HUP
trap '' TSTP

require_root() {
  if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
    echo "${C_RED}${C_BOLD}This script must be run as root.${C_RESET}"
    echo "Run: sudo bash install.sh"
    exit 1
  fi
}

require_systemd() {
  if ! command -v systemctl >/dev/null 2>&1; then
    echo "${C_RED}${C_BOLD}systemctl not found.${C_RESET}"
    echo "This script requires systemd."
    exit 1
  fi
  if [[ ! -d /run/systemd/system ]]; then
    echo "${C_RED}${C_BOLD}systemd is not running (PID 1 is not systemd).${C_RESET}"
    echo "Run this on a normal systemd host (not a minimal container)."
    exit 1
  fi
}

ask_yes_no_tty() {
  local prompt="$1"
  local ans=""
  while true; do
    prompt_set "$prompt"
    if (( TTY_OK )); then
      IFS= read -r -u 3 ans || ans=""
    else
      IFS= read -r ans || ans=""
    fi
    ans="$(printf "%s" "${ans}" | tr -d '\r' | tr '[:upper:]' '[:lower:]' | sed -E 's/^[[:space:]]+|[[:space:]]+$//g')"
    [[ -z "${ans}" ]] && continue
    case "${ans}" in
      y*|yes*) return 0 ;;
      n*|no*)  return 1 ;;
      *)       continue ;;
    esac
  done
}

CURRENT_PCT=0

anim_start() {
  local -n _pid=$1
  local tick=0
  (
    while :; do
      bar_render "${CURRENT_PCT}" "${tick}"
      tick=$((tick+1))
      sleep 0.08
    done
  ) &
  _pid=$!
}

anim_stop() {
  local pid="$1"
  kill "$pid" >/dev/null 2>&1 || true
  wait "$pid" >/dev/null 2>&1 || true
}

monitor_none() { while IFS= read -r _; do :; done; }

run_stream_step() {
  local from="$1"; shift
  local to="$1"; shift
  local msg="$1"; shift
  local monitor_fn="$1"; shift

  local range=$((to - from)); (( range < 0 )) && range=0

  prompt_clear
  status_set "${msg}"
  CURRENT_PCT="${from}"

  local tmpdir fifo
  tmpdir="$(mktemp -d)"
  fifo="${tmpdir}/out.fifo"
  mkfifo "$fifo"

  local anim_pid=0 mon_pid=0 cmd_pid=0 rc=0
  anim_start anim_pid

  (
    "$monitor_fn" "$from" "$to" "$range" < "$fifo"
  ) &
  mon_pid=$!

  local -a cmd
  cmd=("$@")
  local first="${cmd[0]}"
  local kind
  kind="$(type -t "$first" 2>/dev/null || true)"
  if command -v stdbuf >/dev/null 2>&1 && [[ "$kind" == "file" ]]; then
    cmd=(stdbuf -oL -eL "${cmd[@]}")
  fi

  (
    "${cmd[@]}" 2>&1 | tr '\r' '\n' | tee -a "$LOG_FILE" > "$fifo"
  ) &
  cmd_pid=$!

  set +e
  wait "$cmd_pid"
  rc=$?
  set -e

  rm -f "$fifo" >/dev/null 2>&1 || true
  kill "$mon_pid" >/dev/null 2>&1 || true
  wait "$mon_pid" >/dev/null 2>&1 || true
  anim_stop "$anim_pid"
  rm -rf "$tmpdir" >/dev/null 2>&1 || true

  if (( rc != 0 )); then
    echo "---- step failed rc=${rc}: ${msg} ----" >> "${LOG_FILE}"
    fail "$rc"
  fi

  CURRENT_PCT="$to"
  bar_render "$to" 0
}

monitor_apt_update() {
  local from="$1" to="$2" range="$3"
  local stage_m=0
  while IFS= read -r line; do
    case "$line" in
      Hit:*|Get:*|Ign:*|Err:*) (( stage_m < 300 )) && stage_m=300 ;;
      "Reading package lists..."*) (( stage_m < 900 )) && stage_m=900 ;;
      "Reading package lists... Done"*) (( stage_m < 950 )) && stage_m=950 ;;
    esac
    local pct=$(( from + (stage_m * range) / 1000 ))
    (( pct >= to )) && pct=$((to-1))
    (( pct > CURRENT_PCT )) && CURRENT_PCT="$pct"
  done
}

apt_sim_count_install() {
  apt-get -s -o Dpkg::Use-Pty=0 -o Acquire::Retries=3 install --no-install-recommends "$@" 2>/dev/null \
    | awk '/^Inst[[:space:]]/ {c++} END{print c+0}'
}

monitor_apt_install() {
  local from="$1" to="$2" range="$3"
  local total="${APT_TOTAL:-0}"
  local get_seen=0 fetched=0 unpack=0 setup=0

  while IFS= read -r line; do
    case "$line" in
      Hit:*|Get:*|Ign:*|Err:*) ((get_seen++)) ;;
      Fetched*) fetched=1 ;;
      Unpacking\ *) ((unpack++)) ;;
      Setting\ up\ *) ((setup++)) ;;
    esac

    local dl_m=0
    if (( fetched == 1 )); then
      dl_m=200
    else
      local cap=20; local gs=$get_seen
      (( gs > cap )) && gs=$cap
      dl_m=$(( gs * 200 / cap ))
    fi

    local inst_m=0
    if (( total > 0 )); then
      local done=$((unpack + setup))
      local denom=$((2 * total))
      inst_m=$(( done * 800 / denom ))
      (( inst_m > 800 )) && inst_m=800
    else
      (( dl_m > 0 )) && inst_m=50
    fi

    local sub_m=$(( dl_m + inst_m ))
    (( sub_m > 990 )) && sub_m=990

    local pct=$(( from + (sub_m * range) / 1000 ))
    (( pct >= to )) && pct=$((to-1))
    (( pct > CURRENT_PCT )) && CURRENT_PCT="$pct"
  done
}

apt_sim_count_purge() {
  apt-get -s -o Dpkg::Use-Pty=0 -o Acquire::Retries=3 purge "$@" 2>/dev/null \
    | awk '/^Remv[[:space:]]/ {c++} END{print c+0}'
}

monitor_apt_purge() {
  local from="$1" to="$2" range="$3"
  local total="${APT_TOTAL:-0}"
  local removing=0 purging=0 triggers=0

  while IFS= read -r line; do
    case "$line" in
      Removing\ *) ((removing++)) ;;
      Purging\ *) ((purging++)) ;;
      Processing\ triggers\ for\ *) ((triggers++)) ;;
    esac

    local m=0
    if (( total > 0 )); then
      local done=$((removing + purging))
      local denom=$((2 * total))
      m=$(( done * 900 / denom ))
      (( m > 900 )) && m=900
      (( triggers > 0 )) && m=$((m + 50))
    else
      (( removing + purging > 0 )) && m=700
      (( triggers > 0 )) && m=900
    fi
    (( m > 990 )) && m=990

    local pct=$(( from + (m * range) / 1000 ))
    (( pct >= to )) && pct=$((to-1))
    (( pct > CURRENT_PCT )) && CURRENT_PCT="$pct"
  done
}

pkg_installed() {
  dpkg -s "$1" >/dev/null 2>&1
}

version_ge() {
  local current="$1"
  local required="$2"
  [[ "$(printf '%s\n%s\n' "${required}" "${current}" | sort -V | head -n1)" == "${required}" ]]
}

go_version_satisfies() {
  local current=""

  if ! command -v go >/dev/null 2>&1; then
    return 1
  fi

  current="$(go env GOVERSION 2>/dev/null || true)"
  if [[ -z "${current}" ]]; then
    current="$(go version 2>/dev/null | awk '{print $3}' || true)"
  fi
  current="${current#go}"

  [[ -n "${current}" ]] || return 1
  version_ge "${current}" "1.24.0"
}

detect_go_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    armv6l|armv7l) echo "armv6l" ;;
    *)
      echo "Unsupported CPU architecture for Go toolchain install: $(uname -m)" >&2
      return 1
      ;;
  esac
}

install_go_toolchain() {
  local arch tarball url alt_url tmpdir
  arch="$(detect_go_arch)"
  tarball="go${GO_VERSION}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/${tarball}"
  alt_url="https://dl.google.com/go/${tarball}"
  tmpdir="$(mktemp -d)"

  echo "---- install go ${GO_VERSION} (${arch}) ----" >> "${LOG_FILE}"

  if ! curl -fsSL "${url}" -o "${tmpdir}/${tarball}" >> "${LOG_FILE}" 2>&1; then
    curl -fsSL "${alt_url}" -o "${tmpdir}/${tarball}" >> "${LOG_FILE}" 2>&1
  fi

  rm -rf "${GO_INSTALL_ROOT}" >> "${LOG_FILE}" 2>&1 || true
  tar -C /usr/local -xzf "${tmpdir}/${tarball}" >> "${LOG_FILE}" 2>&1
  ln -sf "${GO_INSTALL_ROOT}/bin/go" "${GO_BIN_LINK}" >> "${LOG_FILE}" 2>&1
  ln -sf "${GO_INSTALL_ROOT}/bin/gofmt" "${GOFMT_BIN_LINK}" >> "${LOG_FILE}" 2>&1
  rm -rf "${tmpdir}" >> "${LOG_FILE}" 2>&1 || true

  go_version_satisfies
}

remove_go_toolchain() {
  echo "---- remove go toolchain ----" >> "${LOG_FILE}"
  rm -rf "${GO_INSTALL_ROOT}" >> "${LOG_FILE}" 2>&1 || true
  rm -f "${GO_BIN_LINK}" "${GOFMT_BIN_LINK}" >> "${LOG_FILE}" 2>&1 || true
}

APT_UPDATED=0
apt_update_progress() {
  if (( APT_UPDATED == 1 )); then return 0; fi
  export DEBIAN_FRONTEND=noninteractive
  export NEEDRESTART_MODE=a
  export APT_LISTCHANGES_FRONTEND=none
  echo "---- apt-get update ----" >> "${LOG_FILE}"
  run_stream_step 8 14 "Updating package lists…" monitor_apt_update \
    apt-get -y -o Dpkg::Use-Pty=0 -o Acquire::Retries=3 update
  APT_UPDATED=1
}

apt_install_progress() {
  local from="$1"; shift
  local to="$1"; shift
  local msg="$1"; shift
  export DEBIAN_FRONTEND=noninteractive
  export NEEDRESTART_MODE=a
  export APT_LISTCHANGES_FRONTEND=none
  APT_TOTAL="$(apt_sim_count_install "$@")"
  echo "---- apt-get install: $* (plan=${APT_TOTAL}) ----" >> "${LOG_FILE}"
  run_stream_step "$from" "$to" "$msg" monitor_apt_install \
    apt-get -y -o Dpkg::Use-Pty=0 \
      -o Dpkg::Options::=--force-confdef \
      -o Dpkg::Options::=--force-confold \
      -o Acquire::Retries=3 \
      install --no-install-recommends "$@"
  unset APT_TOTAL
}

apt_purge_progress() {
  local from="$1"; shift
  local to="$1"; shift
  local msg="$1"; shift
  export DEBIAN_FRONTEND=noninteractive
  export NEEDRESTART_MODE=a
  export APT_LISTCHANGES_FRONTEND=none
  APT_TOTAL="$(apt_sim_count_purge "$@")"
  echo "---- apt-get purge: $* (plan=${APT_TOTAL}) ----" >> "${LOG_FILE}"
  run_stream_step "$from" "$to" "$msg" monitor_apt_purge \
    apt-get -y -o Dpkg::Use-Pty=0 -o Acquire::Retries=3 purge "$@"
  unset APT_TOTAL
}

save_state() {
  mkdir -p "${STATE_DIR}"
  chmod 0755 "${STATE_DIR}"
  cat > "${STATE_FILE}" <<EOF
INST_GIT=${INST_GIT:-0}
INST_DOCKER=${INST_DOCKER:-0}
INST_GO=${INST_GO:-0}
EOF
  chmod 0644 "${STATE_FILE}"
}

load_state() {
  INST_GIT=0; INST_DOCKER=0; INST_GO=0
  [[ -f "${STATE_FILE}" ]] || return 0
  while IFS= read -r line; do
    case "$line" in
      INST_GIT=0|INST_GIT=1|INST_DOCKER=0|INST_DOCKER=1|INST_GO=0|INST_GO=1) eval "$line" ;;
    esac
  done < "${STATE_FILE}"
}

ensure_dirs() {
  mkdir -p "${NODE_DIR}" "${GO_DIR}"
  chmod 0750 "${NODE_DIR}" "${GO_DIR}"
}

backup_if_needed() {
  [[ -d "${NODE_DIR}/.git" ]] && return 0
  if [[ -n "$(ls -A "${NODE_DIR}" 2>/dev/null || true)" ]]; then
    local ts; ts="$(date +%Y%m%d-%H%M%S)"
    local bak="${NODE_DIR}.bak-${ts}"
    echo "---- backup ${NODE_DIR} -> ${bak} ----" >> "${LOG_FILE}"
    mv "${NODE_DIR}" "${bak}" >> "${LOG_FILE}" 2>&1
    mkdir -p "${NODE_DIR}"
  fi
}

git_clone_or_update() {
  if [[ -d "${NODE_DIR}/.git" ]]; then
    echo "---- git fetch/reset ----" >> "${LOG_FILE}"
    git -C "${NODE_DIR}" fetch --all --prune >> "${LOG_FILE}" 2>&1
    local def
    def="$(git -C "${NODE_DIR}" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null | sed 's#^origin/##' || true)"
    [[ -z "${def}" ]] && def="main"
    git -C "${NODE_DIR}" reset --hard "origin/${def}" >> "${LOG_FILE}" 2>&1
  else
    backup_if_needed
    echo "---- git clone ----" >> "${LOG_FILE}"
    git clone --depth=1 --progress "${REPO_URL}" "${NODE_DIR}" >> "${LOG_FILE}" 2>&1
  fi
}

ensure_user() {
  if ! id adaemon >/dev/null 2>&1; then
    echo "---- useradd adaemon ----" >> "${LOG_FILE}"
    useradd --system --home-dir "${NODE_DIR}" --shell /usr/sbin/nologin --comment "ADPanel Daemon" adaemon >> "${LOG_FILE}" 2>&1
  fi
  echo "---- perms ----" >> "${LOG_FILE}"
  chown -R adaemon:adaemon "${NODE_DIR}" "${GO_DIR}" >> "${LOG_FILE}" 2>&1
  chmod 0750 "${NODE_DIR}" "${GO_DIR}" >> "${LOG_FILE}" 2>&1
}

docker_enable() {
  echo "---- systemctl enable docker ----" >> "${LOG_FILE}"
  systemctl enable --now docker >> "${LOG_FILE}" 2>&1

  if ! systemctl is-active --quiet docker; then
    echo "---- docker status ----" >> "${LOG_FILE}"
    systemctl status docker --no-pager -l >> "${LOG_FILE}" 2>&1 || true
    echo "---- docker journal ----" >> "${LOG_FILE}"
    journalctl -u docker -n 200 --no-pager >> "${LOG_FILE}" 2>&1 || true
    return 1
  fi

  if ! getent group docker >/dev/null 2>&1; then
    echo "---- groupadd docker ----" >> "${LOG_FILE}"
    groupadd docker >> "${LOG_FILE}" 2>&1 || true
  fi

  if id adaemon >/dev/null 2>&1; then
    echo "---- usermod docker group ----" >> "${LOG_FILE}"
    usermod -aG docker adaemon >> "${LOG_FILE}" 2>&1 || true
  fi
}

detect_build_target() {
  if [[ -f "${NODE_DIR}/main.go" ]]; then echo "."; return; fi
  if compgen -G "${NODE_DIR}/cmd/*/main.go" >/dev/null 2>&1; then
    local f; f="$(ls -1 "${NODE_DIR}"/cmd/*/main.go 2>/dev/null | head -n1)"
    echo "./$(dirname "${f#"${NODE_DIR}/"}")"
    return
  fi
  local found
  found="$(grep -Rsl --include='*.go' '^package main' "${NODE_DIR}" 2>/dev/null | head -n1 || true)"
  if [[ -n "${found}" ]]; then
    echo "./$(dirname "${found#"${NODE_DIR}/"}")"
    return
  fi
  echo "."
}

build_binary_as_adaemon() {
  local target; target="$(detect_build_target)"
  mkdir -p "${GO_DIR}/gopath" "${GO_DIR}/gocache" "${GO_DIR}/gomodcache"
  chown -R adaemon:adaemon "${GO_DIR}"

  echo "---- go build target=${target} ----" >> "${LOG_FILE}"
  runuser -u adaemon -- bash -lc "
    set -eEu
    set -o pipefail
    export HOME='${NODE_DIR}'
    export GOPATH='${GO_DIR}/gopath'
    export GOCACHE='${GO_DIR}/gocache'
    export GOMODCACHE='${GO_DIR}/gomodcache'
    cd '${NODE_DIR}'
    go build -trimpath -buildvcs=false -ldflags '-s -w' -o '${GO_DIR}/adnode' '${target}'
  " </dev/null >> "${LOG_FILE}" 2>&1

  echo "---- install binary ----" >> "${LOG_FILE}"
  install -m 0755 -o root -g root "${GO_DIR}/adnode" "${BIN_DST}" >> "${LOG_FILE}" 2>&1
}

cleanup_legacy_unit() {
  echo "---- remove legacy unit ${LEGACY_UNIT_NAME} ----" >> "${LOG_FILE}"
  systemctl stop "${LEGACY_UNIT_NAME}" >> "${LOG_FILE}" 2>&1 || true
  systemctl disable "${LEGACY_UNIT_NAME}" >> "${LOG_FILE}" 2>&1 || true
  rm -f "${LEGACY_UNIT_PATH}" >> "${LOG_FILE}" 2>&1 || true
}

write_systemd_unit() {
  cleanup_legacy_unit
  echo "---- write systemd unit ----" >> "${LOG_FILE}"
  cat > "${UNIT_PATH}" <<UNIT
[Unit]
Description=ADaemon
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
User=adaemon
Group=adaemon
SupplementaryGroups=docker
WorkingDirectory=/var/lib/node
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

ExecStart=/usr/local/bin/adnode
Restart=on-failure
RestartSec=2
TimeoutStartSec=30
TimeoutStopSec=30
KillSignal=SIGTERM
UMask=0027
LimitNOFILE=1048576

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectControlGroups=true
ProtectKernelTunables=true
ProtectKernelLogs=true
ProtectKernelModules=true
LockPersonality=true
RestrictSUIDSGID=true
RestrictRealtime=true
SystemCallArchitectures=native

ReadWritePaths=/var/lib/node /var/lib/gonode /var/run/docker.sock

[Install]
WantedBy=multi-user.target
UNIT

  echo "---- systemctl daemon-reload/enable ----" >> "${LOG_FILE}"
  systemctl daemon-reload >> "${LOG_FILE}" 2>&1
  systemctl enable "${UNIT_NAME}" >> "${LOG_FILE}" 2>&1
}

START_WARN=0
start_and_verify() {
  echo "---- systemctl restart ----" >> "${LOG_FILE}"
  systemctl restart "${UNIT_NAME}" >> "${LOG_FILE}" 2>&1 || true

  if systemctl is-active --quiet "${UNIT_NAME}"; then
    return 0
  fi

  echo "---- systemctl status ----" >> "${LOG_FILE}"
  systemctl status "${UNIT_NAME}" --no-pager -l >> "${LOG_FILE}" 2>&1 || true
  echo "---- journalctl ----" >> "${LOG_FILE}"
  journalctl -u "${UNIT_NAME}" -n 200 --no-pager >> "${LOG_FILE}" 2>&1 || true

  if [[ ! -f "${NODE_DIR}/config.yml" ]]; then
    START_WARN=1
    return 0
  fi
  return 1
}

stop_disable_service() {
  echo "---- stop/disable service ----" >> "${LOG_FILE}"
  systemctl stop "${UNIT_NAME}" >> "${LOG_FILE}" 2>&1 || true
  systemctl disable "${UNIT_NAME}" >> "${LOG_FILE}" 2>&1 || true
  systemctl stop "${LEGACY_UNIT_NAME}" >> "${LOG_FILE}" 2>&1 || true
  systemctl disable "${LEGACY_UNIT_NAME}" >> "${LOG_FILE}" 2>&1 || true
  systemctl daemon-reload >> "${LOG_FILE}" 2>&1 || true
}

remove_files_users() {
  echo "---- remove unit/binary/dirs/user ----" >> "${LOG_FILE}"
  rm -f "${UNIT_PATH}" >> "${LOG_FILE}" 2>&1 || true
  rm -f "${LEGACY_UNIT_PATH}" >> "${LOG_FILE}" 2>&1 || true
  rm -f "${BIN_DST}" >> "${LOG_FILE}" 2>&1 || true
  rm -rf "${NODE_DIR}" "${GO_DIR}" >> "${LOG_FILE}" 2>&1 || true
  userdel adaemon >> "${LOG_FILE}" 2>&1 || true
  rm -rf "${STATE_DIR}" >> "${LOG_FILE}" 2>&1 || true
  systemctl daemon-reload >> "${LOG_FILE}" 2>&1 || true
}

require_root
require_systemd
mkdir -p "$(dirname "${LOG_FILE}")"
: > "${LOG_FILE}"

ui_splash
status_set "Ready."
CURRENT_PCT=0
bar_render 0 0

INSTALL_CHOSEN=0
UNINSTALL_CHOSEN=0

if (( ASSUME_YES == 1 )); then
  INSTALL_CHOSEN=1
  UNINSTALL_CHOSEN=0
else
  if (( TTY_OK == 0 )) && [[ ! -t 0 ]]; then
    echo
    echo "${C_RED}${C_BOLD}No interactive TTY available for questions.${C_RESET}"
    echo "Re-run with:"
    echo "  curl -fsSL 'https://bash.ad-panel.com/install.sh?nocache=$(date +%s)' | tr -d '\r' | sudo bash -s -- --yes"
    exit 1
  fi

  if ask_yes_no_tty "1) Do you want to install the daemon?"; then
    INSTALL_CHOSEN=1
  else
    INSTALL_CHOSEN=0
  fi

  if ask_yes_no_tty "2) Do you want to uninstall the panel?"; then
    UNINSTALL_CHOSEN=1
  else
    UNINSTALL_CHOSEN=0
  fi
fi

prompt_clear
status_set "Booting…"
CURRENT_PCT=2
bar_render 2 0

if (( INSTALL_CHOSEN == 1 && UNINSTALL_CHOSEN == 1 )); then
  prompt_clear
  status_set "Only one option can be selected at a time (install OR uninstall)."
  ui_bye
  exit 1
fi

if (( INSTALL_CHOSEN == 0 && UNINSTALL_CHOSEN == 0 )); then
  ui_bye
  exit 0
fi

if ! command -v apt-get >/dev/null 2>&1; then
  echo "${C_RED}${C_BOLD}Unsupported system: only apt-based distros (Ubuntu/Debian) are supported.${C_RESET}"
  exit 1
fi

if (( UNINSTALL_CHOSEN == 1 )); then
  load_state

  run_stream_step 5  15 "Stopping daemon…"                   monitor_none stop_disable_service
  run_stream_step 15 35 "Removing files and user…"           monitor_none remove_files_users

  if (( INST_GO == 1 )); then
    run_stream_step 35 45 "Removing Go toolchain files…"        monitor_none remove_go_toolchain
    apt_purge_progress 45 55 "Removing Go toolchain package…" golang-go
  fi
  if (( INST_DOCKER == 1 )); then
    apt_purge_progress 55 78 "Removing Docker (docker.io)…" docker.io
  fi
  if (( INST_GIT == 1 )); then
    apt_purge_progress 78 90 "Removing Git (git)…" git
  fi

  run_stream_step 90 96 "Cleaning up (autoremove)…"          monitor_none apt-get -y -o Dpkg::Use-Pty=0 autoremove
  run_stream_step 96 100 "Finishing…"                         monitor_none true

  prompt_clear
  status_set "✓ Uninstall complete."
  CURRENT_PCT=100
  bar_render 100 0

  if (( UI )); then
    box_row=$((ROW_STATUS+2))
    center_at_plain "$box_row"       "┌──────────────────────────────────────────────┐" "${C_DIM}" "${C_RESET}"
    center_at_plain "$((box_row+1))" "│  ADaemon has been removed.                   │" "${C_WHITE}" "${C_RESET}"
    center_at_plain "$((box_row+2))" "│  Service, binary, user, and directories gone.│" "${C_WHITE}" "${C_RESET}"
    center_at_plain "$((box_row+3))" "└──────────────────────────────────────────────┘" "${C_DIM}" "${C_RESET}"
    cup "$((box_row+5))" 1
  fi

  exit 0
fi

INST_GIT=0
INST_DOCKER=0
INST_GO=0

prompt_clear
status_set "Preparing…"
CURRENT_PCT=3
bar_render 3 0

apt_update_progress

run_stream_step 14 18 "Preparing directories…"              monitor_none ensure_dirs

missing_core=()
for p in ca-certificates curl sed grep util-linux; do
  pkg_installed "$p" || missing_core+=("$p")
done
if (( ${#missing_core[@]} > 0 )); then
  apt_install_progress 18 30 "Installing core dependencies…" "${missing_core[@]}"
else
  run_stream_step 18 30 "Core dependencies already present…" monitor_none true
fi

if ! command -v git >/dev/null 2>&1; then
  pkg_installed git || INST_GIT=1
  apt_install_progress 30 38 "Installing Git…" git
else
  run_stream_step 30 38 "Git already installed…" monitor_none true
fi

run_stream_step 38 50 "Deploying daemon source…"            monitor_none git_clone_or_update

run_stream_step 50 56 "Creating restricted user (adaemon)…" monitor_none ensure_user

if ! command -v docker >/dev/null 2>&1; then
  pkg_installed docker.io || INST_DOCKER=1
  apt_install_progress 56 72 "Installing Docker (docker.io)…" docker.io
else
  run_stream_step 56 72 "Docker already installed…" monitor_none true
fi
run_stream_step 72 78 "Enabling Docker service…"            monitor_none docker_enable

if ! go_version_satisfies; then
  INST_GO=1
  run_stream_step 78 88 "Installing Go toolchain (Go ${GO_VERSION})…" monitor_none install_go_toolchain
else
  run_stream_step 78 88 "Go already installed…" monitor_none true
fi

run_stream_step 88 95 "Building & installing daemon…"       monitor_none build_binary_as_adaemon
run_stream_step 95 98 "Creating systemd service…"           monitor_none write_systemd_unit
run_stream_step 98 100 "Starting daemon (best-effort)…"     monitor_none start_and_verify

save_state

prompt_clear
if (( START_WARN == 1 )); then
  status_set "✓ Install complete (daemon NOT started: missing config.yml)."
else
  status_set "✓ Install complete."
fi
CURRENT_PCT=100
bar_render 100 0

if (( UI )); then
  box_row=$((ROW_STATUS+2))
  center_at_plain "$box_row"       "┌──────────────────────────────────────────────┐" "${C_DIM}" "${C_RESET}"
  center_at_plain "$((box_row+1))" "│  Service control (systemd)                   │" "${C_WHITE}" "${C_RESET}"
  center_at_plain "$((box_row+2))" "├──────────────────────────────────────────────┤" "${C_DIM}" "${C_RESET}"
  center_at_plain "$((box_row+3))" "│  Start:    systemctl start ADaemon           │" "${C_WHITE}" "${C_RESET}"
  center_at_plain "$((box_row+4))" "│  Stop:     systemctl stop ADaemon            │" "${C_WHITE}" "${C_RESET}"
  center_at_plain "$((box_row+5))" "│  Restart:  systemctl restart ADaemon         │" "${C_WHITE}" "${C_RESET}"
  center_at_plain "$((box_row+6))" "│  Status:   systemctl status ADaemon          │" "${C_WHITE}" "${C_RESET}"
  center_at_plain "$((box_row+7))" "└──────────────────────────────────────────────┘" "${C_DIM}" "${C_RESET}"

  if (( START_WARN == 1 )); then
    center_at_plain "$((box_row+9))" "Create /var/lib/node/config.yml then start the service." "${C_YELLOW}${C_BOLD}" "${C_RESET}"
  else
    center_at_plain "$((box_row+9))" "Tip: config at /var/lib/node/config.yml" "${C_DIM}" "${C_RESET}"
  fi

  cup "$((box_row+11))" 1
else
  echo
  echo "Service control:"
  echo "  Start: systemctl start ADaemon"
  echo "  Stop: systemctl stop ADaemon"
  echo "  Restart: systemctl restart ADaemon"
  echo "  Status: systemctl status ADaemon"
  echo
  echo "Config: /var/lib/node/config.yml"
  if (( START_WARN == 1 )); then
    echo "NOTE: daemon not started because config.yml is missing."
  fi
fi
