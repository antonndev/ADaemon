#!/bin/sh
set -eu

installer_source_dir="$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd -P || pwd)"
export ADPANEL_INSTALLER_SOURCE_DIR="${ADPANEL_INSTALLER_SOURCE_DIR:-$installer_source_dir}"

as_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    echo "This installer needs root. Re-run it as root or install sudo." >&2
    exit 1
  fi
}

install_bash() {
  if command -v bash >/dev/null 2>&1; then
    return 0
  fi
  echo "Installing bash for the ADaemon installer..."
  if command -v apt-get >/dev/null 2>&1; then
    as_root apt-get update
    as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y bash
  elif command -v dnf >/dev/null 2>&1; then
    as_root dnf install -y bash
  elif command -v yum >/dev/null 2>&1; then
    as_root yum install -y bash
  elif command -v apk >/dev/null 2>&1; then
    as_root apk add --no-cache bash
  elif command -v pacman >/dev/null 2>&1; then
    as_root pacman -Sy --noconfirm --needed bash
  elif command -v zypper >/dev/null 2>&1; then
    as_root zypper --non-interactive install -y bash
  else
    echo "Unsupported package manager: cannot install bash." >&2
    exit 1
  fi
}

install_bash

tmp_script="${TMPDIR:-/tmp}/adpanel-node-install-$$.bash"
cat > "$tmp_script" <<'ADPANEL_NODE_INSTALLER'
#!/usr/bin/env bash
set -Eeuo pipefail

CONFIG_B64="${ADPANEL_NODE_CONFIG_B64:-}"
CONFIG_URL="${ADPANEL_NODE_CONFIG_URL:-}"
CONFIG_HEADER="${ADPANEL_NODE_CONFIG_HEADER:-}"
NODE_API_PORT="${ADPANEL_NODE_API_PORT:-8080}"
NODE_SFTP_PORT="${ADPANEL_NODE_SFTP_PORT:-2022}"

REPO_URL="https://github.com/antonndev/ADaemon.git"
NODE_DIR="/var/lib/node"
GO_DIR="/var/lib/gonode"
GO_VERSION="1.24.11"
GO_INSTALL_ROOT="/usr/local/go"
BIN_DST="/usr/local/bin/adnode"
SERVICE_NAME="ADaemon"
ALIAS_SERVICE_NAME="adaemon"
LOG_FILE="/var/log/adpanel-daemon-install.log"
PM=""
OS_ID=""
OS_LIKE=""
OS_VERSION_ID=""
OS_MAJOR=""

log() { printf '\n==> %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
systemd_running() { have systemctl && [ -d /run/systemd/system ]; }

require_root() {
  if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    die "Run this installer as root, for example: sudo sh install-daemon.sh"
  fi
}

detect_platform() {
  if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_ID="${ID:-}"
    OS_LIKE="${ID_LIKE:-}"
    OS_VERSION_ID="${VERSION_ID:-}"
  fi
  OS_MAJOR="${OS_VERSION_ID%%.*}"
  if have apt-get; then PM="apt"
  elif have dnf; then PM="dnf"
  elif have yum; then PM="yum"
  elif have apk; then PM="apk"
  elif have pacman; then PM="pacman"
  elif have zypper; then PM="zypper"
  else die "Unsupported Linux package manager. Need apt, dnf, yum, apk, pacman, or zypper."
  fi
  log "Detected package manager: $PM"
}

pkg_update() {
  case "$PM" in
    apt)
      export DEBIAN_FRONTEND=noninteractive
      apt-get -y -o Dpkg::Use-Pty=0 -o DPkg::Lock::Timeout=300 -o Acquire::Retries=3 update
      ;;
    dnf) dnf -y makecache --refresh || dnf -y makecache || true ;;
    yum) yum -y makecache || true ;;
    apk) apk update ;;
    pacman) pacman -Sy --noconfirm ;;
    zypper) zypper --non-interactive refresh || true ;;
  esac
}

pkg_install() {
  case "$PM" in
    apt)
      export DEBIAN_FRONTEND=noninteractive
      apt-get -y -o Dpkg::Use-Pty=0 -o DPkg::Lock::Timeout=300 -o Acquire::Retries=3 install --no-install-recommends "$@"
      ;;
    dnf) dnf install -y "$@" ;;
    yum) yum install -y "$@" ;;
    apk) apk add --no-cache "$@" ;;
    pacman) pacman -S --noconfirm --needed "$@" ;;
    zypper) zypper --non-interactive install -y "$@" ;;
  esac
}

install_base_packages() {
  log "Installing base dependencies..."
  pkg_update
  case "$PM" in
    apt)
      pkg_install ca-certificates curl git tar gzip sed grep coreutils util-linux passwd procps
      ;;
    dnf|yum)
      pkg_install ca-certificates curl git tar gzip sed grep coreutils util-linux shadow-utils procps-ng findutils
      ;;
    apk)
      pkg_install ca-certificates curl git tar gzip sed grep coreutils util-linux
      pkg_install shadow >/dev/null 2>&1 || true
      ;;
    pacman)
      pkg_install ca-certificates curl git tar gzip sed grep coreutils util-linux shadow procps-ng
      ;;
    zypper)
      pkg_install ca-certificates curl git tar gzip sed grep coreutils util-linux shadow procps
      ;;
  esac
  update-ca-certificates >/dev/null 2>&1 || true
}

install_docker_convenience() {
  log "Trying Docker official convenience installer..."
  curl -fsSL https://get.docker.com -o /tmp/get-docker.sh
  sh /tmp/get-docker.sh
}

enable_rhel_docker_repo() {
  pkg_install dnf-plugins-core >/dev/null 2>&1 || true
  pkg_install yum-utils >/dev/null 2>&1 || true
  pkg_install epel-release >/dev/null 2>&1 || true
  if have crb; then
    crb enable >/dev/null 2>&1 || true
  fi
  if have dnf; then
    dnf config-manager --set-enabled crb >/dev/null 2>&1 || true
    dnf config-manager --set-enabled powertools >/dev/null 2>&1 || true
    dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo >/dev/null 2>&1 || true
  fi
  if have yum-config-manager; then
    yum-config-manager --enable crb >/dev/null 2>&1 || true
    yum-config-manager --enable powertools >/dev/null 2>&1 || true
    yum-config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo >/dev/null 2>&1 || true
  fi
}

install_docker() {
  if have docker; then
    log "Docker already installed."
    return 0
  fi

  log "Installing Docker..."
  case "$PM" in
    apt)
      pkg_install docker.io || install_docker_convenience
      ;;
    dnf|yum)
      enable_rhel_docker_repo
      pkg_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
        || pkg_install moby-engine docker-cli containerd \
        || pkg_install docker \
        || install_docker_convenience
      ;;
    apk)
      pkg_install docker docker-cli || pkg_install docker
      ;;
    pacman)
      pkg_install docker
      ;;
    zypper)
      pkg_install docker
      ;;
  esac

  have docker || die "Docker installation finished but docker command was not found."
}

ensure_docker_group() {
  if ! getent group docker >/dev/null 2>&1; then
    if have groupadd; then groupadd docker >/dev/null 2>&1 || true
    elif have addgroup; then addgroup -S docker >/dev/null 2>&1 || addgroup docker >/dev/null 2>&1 || true
    fi
  fi
  if id adaemon >/dev/null 2>&1; then
    if have usermod; then usermod -aG docker adaemon >/dev/null 2>&1 || true
    elif have addgroup; then addgroup adaemon docker >/dev/null 2>&1 || true
    fi
  fi
}

start_docker() {
  if docker info >/dev/null 2>&1; then
    log "Docker is already running."
    return 0
  fi

  log "Starting Docker service..."
  if systemd_running; then
    systemctl enable --now docker >/dev/null 2>&1 || systemctl start docker
  elif have rc-service; then
    rc-update add docker default >/dev/null 2>&1 || true
    rc-service docker start
  elif have service; then
    service docker start
  elif have dockerd; then
    nohup dockerd >/var/log/dockerd.log 2>&1 &
  fi

  for _ in $(seq 1 30); do
    if docker info >/dev/null 2>&1; then
      log "Docker is running."
      return 0
    fi
    sleep 1
  done
  die "Docker is installed but did not start. Check Docker service logs."
}

go_version_satisfies() {
  have go || return 1
  local raw major rest minor
  raw="$(go env GOVERSION 2>/dev/null || go version 2>/dev/null | awk '{print $3}' || true)"
  raw="${raw#go}"
  major="${raw%%.*}"
  rest="${raw#*.}"
  minor="${rest%%.*}"
  [[ "$major" =~ ^[0-9]+$ && "$minor" =~ ^[0-9]+$ ]] || return 1
  (( major > 1 || (major == 1 && minor >= 24) ))
}

detect_go_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    armv6l|armv7l) echo "armv6l" ;;
    *) die "Unsupported CPU architecture for Go: $(uname -m)" ;;
  esac
}

install_go_toolchain() {
  if go_version_satisfies; then
    log "Go $(go env GOVERSION 2>/dev/null || go version | awk '{print $3}') already installed."
    return 0
  fi

  local arch tarball url alt_url tmpdir
  arch="$(detect_go_arch)"
  tarball="go$GO_VERSION.linux-$arch.tar.gz"
  url="https://go.dev/dl/$tarball"
  alt_url="https://dl.google.com/go/$tarball"
  tmpdir="$(mktemp -d)"

  log "Installing Go $GO_VERSION ($arch)..."
  if ! curl -fsSL "$url" -o "$tmpdir/$tarball"; then
    curl -fsSL "$alt_url" -o "$tmpdir/$tarball"
  fi
  rm -rf "$GO_INSTALL_ROOT"
  tar -C /usr/local -xzf "$tmpdir/$tarball"
  ln -sf "$GO_INSTALL_ROOT/bin/go" /usr/local/bin/go
  ln -sf "$GO_INSTALL_ROOT/bin/gofmt" /usr/local/bin/gofmt
  rm -rf "$tmpdir"
  go_version_satisfies || die "Installed Go does not satisfy ADaemon requirements."
}

backup_non_git_node_dir() {
  [ -d "$NODE_DIR" ] || return 0
  [ -d "$NODE_DIR/.git" ] && return 0
  if [ -n "$(find "$NODE_DIR" -mindepth 1 -maxdepth 1 2>/dev/null | head -n 1)" ]; then
    local backup
    backup="$NODE_DIR.bak-$(date +%Y%m%d-%H%M%S)"
    log "Existing non-git node directory found; moving it to $backup"
    mv "$NODE_DIR" "$backup"
  fi
}

deploy_source() {
  log "Deploying ADaemon source..."
  mkdir -p "$(dirname "$NODE_DIR")" "$GO_DIR"
  if [ -d "$NODE_DIR/.git" ]; then
    git -C "$NODE_DIR" remote set-url origin "$REPO_URL" >/dev/null 2>&1 || true
    git -C "$NODE_DIR" fetch --all --prune
    local branch
    branch="$(git -C "$NODE_DIR" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null | sed 's#^origin/##' || true)"
    [ -n "$branch" ] || branch="main"
    git -C "$NODE_DIR" reset --hard "origin/$branch"
  else
    backup_non_git_node_dir
    git clone --depth=1 "$REPO_URL" "$NODE_DIR"
  fi
}

write_config() {
  mkdir -p "$NODE_DIR"

  if [ -n "$CONFIG_B64" ]; then
    log "Writing /var/lib/node/config.yml from embedded panel config..."
    if base64 -d </dev/null >/dev/null 2>&1; then
      printf '%s' "$CONFIG_B64" | base64 -d > "$NODE_DIR/config.yml"
    else
      printf '%s' "$CONFIG_B64" | base64 --decode > "$NODE_DIR/config.yml"
    fi
  elif [ -n "$CONFIG_URL" ]; then
    log "Downloading /var/lib/node/config.yml from panel..."
    if [ -n "$CONFIG_HEADER" ]; then
      curl -fsSL -H "$CONFIG_HEADER" "$CONFIG_URL" -o "$NODE_DIR/config.yml"
    else
      curl -fsSL "$CONFIG_URL" -o "$NODE_DIR/config.yml"
    fi
  elif [ -f "$NODE_DIR/config.yml" ]; then
    log "Keeping existing /var/lib/node/config.yml."
  else
    warn "No node config provided. Create /var/lib/node/config.yml before starting ADaemon."
    return 0
  fi

  chmod 0640 "$NODE_DIR/config.yml" 2>/dev/null || true
}

apply_docker_compat_patch() {
  rm -f "$NODE_DIR/docker_types_compat.go"
  if grep -Rqs "type dockerHostConfig" "$NODE_DIR"/*.go 2>/dev/null && grep -Rqs "func (d Docker) engineRequest" "$NODE_DIR"/*.go 2>/dev/null; then
    return 0
  fi

  if [ -n "${ADPANEL_INSTALLER_SOURCE_DIR:-}" ] && [ -f "$ADPANEL_INSTALLER_SOURCE_DIR/docker_engine.go" ]; then
    log "Copying Docker API compatibility file into /var/lib/node..."
    cp "$ADPANEL_INSTALLER_SOURCE_DIR/docker_engine.go" "$NODE_DIR/docker_engine.go"
    return 0
  fi

  log "Applying ADaemon Docker API compatibility patch..."
  cat > "$NODE_DIR/docker_types_compat.go" <<'GO_COMPAT'
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type dockerPortBinding struct {
	HostIP   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort,omitempty"`
}

type dockerRestartPolicy struct {
	Name              string `json:"Name,omitempty"`
	MaximumRetryCount int    `json:"MaximumRetryCount,omitempty"`
}

type dockerUlimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

type dockerHostConfig struct {
	Binds             []string                       `json:"Binds,omitempty"`
	PortBindings      map[string][]dockerPortBinding `json:"PortBindings,omitempty"`
	RestartPolicy     dockerRestartPolicy            `json:"RestartPolicy,omitempty"`
	CapDrop           []string                       `json:"CapDrop,omitempty"`
	CapAdd            []string                       `json:"CapAdd,omitempty"`
	SecurityOpt       []string                       `json:"SecurityOpt,omitempty"`
	PidsLimit         int64                          `json:"PidsLimit,omitempty"`
	ReadonlyRootfs    bool                           `json:"ReadonlyRootfs,omitempty"`
	Tmpfs             map[string]string              `json:"Tmpfs,omitempty"`
	Memory            int64                          `json:"Memory,omitempty"`
	MemoryReservation int64                          `json:"MemoryReservation,omitempty"`
	MemorySwap        int64                          `json:"MemorySwap,omitempty"`
	CpuPeriod         int64                          `json:"CpuPeriod,omitempty"`
	CpuQuota          int64                          `json:"CpuQuota,omitempty"`
	CpuShares         int64                          `json:"CpuShares,omitempty"`
	BlkioWeight       int64                          `json:"BlkioWeight,omitempty"`
	Ulimits           []dockerUlimit                 `json:"Ulimits,omitempty"`
}

type dockerContainerCreateRequest struct {
	Image        string              `json:"Image"`
	Env          []string            `json:"Env,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	User         string              `json:"User,omitempty"`
	Tty          bool                `json:"Tty,omitempty"`
	OpenStdin    bool                `json:"OpenStdin,omitempty"`
	AttachStdin  bool                `json:"AttachStdin,omitempty"`
	AttachStdout bool                `json:"AttachStdout,omitempty"`
	AttachStderr bool                `json:"AttachStderr,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   dockerHostConfig    `json:"HostConfig,omitempty"`
}

type dockerImageInspectResponse struct {
	Config struct {
		Volumes    map[string]any `json:"Volumes"`
		WorkingDir string         `json:"WorkingDir"`
		User       string         `json:"User"`
		Env        []string       `json:"Env"`
		Entrypoint []string       `json:"Entrypoint"`
		Cmd        []string       `json:"Cmd"`
	} `json:"Config"`
}

type dockerContainerInspectResponse struct {
	HostConfig dockerHostConfig `json:"HostConfig"`
}

func decodeDockerAPIError(body []byte) string {
	var payload struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &payload) == nil {
		if msg := strings.TrimSpace(payload.Message); msg != "" {
			return msg
		}
		if msg := strings.TrimSpace(payload.Error); msg != "" {
			return msg
		}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return "Docker API request failed"
	}
	if len(msg) > 800 {
		msg = msg[:800]
	}
	return msg
}

func dockerHTTPClient() *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", "/var/run/docker.sock")
		},
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute}
}

func (d Docker) engineRequest(ctx context.Context, method, endpoint string, query url.Values, body any) ([]byte, int, error) {
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(payload)
	}
	reqURL := "http://docker" + endpoint
	if query != nil && len(query) > 0 {
		reqURL += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, reader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := dockerHTTPClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
	if readErr != nil {
		return nil, resp.StatusCode, readErr
	}
	return data, resp.StatusCode, nil
}

func (d Docker) runContainerAPI(ctx context.Context, name string, req dockerContainerCreateRequest) error {
	query := url.Values{}
	query.Set("name", name)
	body, status, err := d.engineRequest(ctx, http.MethodPost, "/containers/create", query, req)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("failed to create container %s: %s", name, decodeDockerAPIError(body))
	}
	body, status, err = d.engineRequest(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotModified {
		return fmt.Errorf("failed to start container %s: %s", name, decodeDockerAPIError(body))
	}
	return nil
}

func (d Docker) pullImageAPI(ctx context.Context, imageRef string) error {
	_, stderr, code, err := d.runCollect(ctx, "pull", imageRef)
	if err != nil || code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if msg == "" {
			msg = "docker pull failed"
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func (d Docker) inspectImageConfigAPI(ctx context.Context, ref string) (imageConfig, error) {
	body, status, err := d.engineRequest(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil)
	if err != nil {
		return imageConfig{}, err
	}
	if status < 200 || status >= 300 {
		return imageConfig{}, fmt.Errorf("failed to inspect image %s: %s", ref, decodeDockerAPIError(body))
	}
	var payload dockerImageInspectResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return imageConfig{}, err
	}
	cfg := imageConfig{Env: map[string]string{}}
	for path := range payload.Config.Volumes {
		path = strings.TrimSpace(path)
		if path != "" && path != "/" {
			cfg.Volumes = append(cfg.Volumes, path)
		}
	}
	cfg.Workdir = strings.TrimSpace(payload.Config.WorkingDir)
	cfg.User = strings.TrimSpace(payload.Config.User)
	for _, kv := range payload.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			cfg.Env[k] = v
		}
	}
	cfg.Entrypoint = payload.Config.Entrypoint
	cfg.Cmd = payload.Config.Cmd
	return cfg, nil
}

func (d Docker) inspectContainerAPI(ctx context.Context, name string) (dockerContainerInspectResponse, error) {
	body, status, err := d.engineRequest(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, nil)
	if err != nil {
		return dockerContainerInspectResponse{}, err
	}
	if status < 200 || status >= 300 {
		return dockerContainerInspectResponse{}, fmt.Errorf("failed to inspect container %s: %s", name, decodeDockerAPIError(body))
	}
	var payload dockerContainerInspectResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return dockerContainerInspectResponse{}, err
	}
	return payload, nil
}
GO_COMPAT
}

ensure_user() {
  log "Ensuring adaemon system user..."
  if ! id adaemon >/dev/null 2>&1; then
    if have useradd; then
      local nologin="/usr/sbin/nologin"
      [ -x "$nologin" ] || nologin="/sbin/nologin"
      [ -x "$nologin" ] || nologin="/bin/false"
      useradd --system --home-dir "$NODE_DIR" --shell "$nologin" --comment "ADPanel Daemon" adaemon
    elif have adduser; then
      addgroup -S adaemon >/dev/null 2>&1 || addgroup adaemon >/dev/null 2>&1 || true
      adduser -S -D -H -h "$NODE_DIR" -s /sbin/nologin -G adaemon adaemon >/dev/null 2>&1 \
        || adduser -D -H -h "$NODE_DIR" -s /sbin/nologin -G adaemon adaemon
    else
      die "Cannot create adaemon user: useradd/adduser not found."
    fi
  fi
  ensure_docker_group
  chown -R adaemon:adaemon "$NODE_DIR" "$GO_DIR" 2>/dev/null || chown -R adaemon "$NODE_DIR" "$GO_DIR"
  chmod 0750 "$NODE_DIR" "$GO_DIR"
  chmod 0640 "$NODE_DIR/config.yml" 2>/dev/null || true
}

build_binary() {
  log "Building ADaemon..."
  mkdir -p "$GO_DIR/gopath" "$GO_DIR/gocache" "$GO_DIR/gomodcache"
  cd "$NODE_DIR"
  export GOPATH="$GO_DIR/gopath"
  export GOCACHE="$GO_DIR/gocache"
  export GOMODCACHE="$GO_DIR/gomodcache"
  go build -trimpath -buildvcs=false -ldflags "-s -w" -o "$GO_DIR/adnode" .
  install -m 0755 -o root -g root "$GO_DIR/adnode" "$BIN_DST"
}

write_systemd_service() {
  log "Writing systemd service..."
  cat > "/etc/systemd/system/$SERVICE_NAME.service" <<'UNIT'
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
  ln -sf "$SERVICE_NAME.service" "/etc/systemd/system/$ALIAS_SERVICE_NAME.service"
  rm -f /etc/systemd/system/adpanel-daemon.service
  systemctl daemon-reload
  systemctl enable "$SERVICE_NAME.service"
}

write_openrc_service() {
  log "Writing OpenRC service..."
  cat > /etc/init.d/adaemon <<'RC'
#!/sbin/openrc-run
name="ADaemon"
description="ADPanel Daemon"
command="/usr/local/bin/adnode"
command_user="adaemon:adaemon"
directory="/var/lib/node"
pidfile="/run/adaemon.pid"
command_background="yes"
output_log="/var/log/adaemon.log"
error_log="/var/log/adaemon.err"

depend() {
  need net
  after docker
}
RC
  chmod +x /etc/init.d/adaemon
  rc-update add adaemon default >/dev/null 2>&1 || true
}

write_sysv_service() {
  log "Writing SysV service..."
  cat > /etc/init.d/adaemon <<'INIT'
#!/bin/sh
### BEGIN INIT INFO
# Provides:          adaemon
# Required-Start:    $remote_fs $network docker
# Required-Stop:     $remote_fs $network
# Default-Start:     2 3 4 5
# Default-Stop:      0 1 6
# Short-Description: ADPanel Daemon
### END INIT INFO

PIDFILE=/run/adaemon.pid
DAEMON=/usr/local/bin/adnode
WORKDIR=/var/lib/node
USER=adaemon

start_service() {
  if command -v start-stop-daemon >/dev/null 2>&1; then
    start-stop-daemon --start --background --make-pidfile --pidfile "$PIDFILE" --chuid "$USER" --chdir "$WORKDIR" --exec "$DAEMON"
  else
    su -s /bin/sh "$USER" -c "cd '$WORKDIR' && nohup '$DAEMON' >/var/log/adaemon.log 2>/var/log/adaemon.err & echo \$! > '$PIDFILE'"
  fi
}

stop_service() {
  if [ -f "$PIDFILE" ]; then
    kill "$(cat "$PIDFILE")" 2>/dev/null || true
    rm -f "$PIDFILE"
  fi
}

case "$1" in
  start) start_service ;;
  stop) stop_service ;;
  restart) stop_service; sleep 1; start_service ;;
  status) [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null ;;
  *) echo "Usage: $0 {start|stop|restart|status}"; exit 1 ;;
esac
INIT
  chmod +x /etc/init.d/adaemon
  if have update-rc.d; then update-rc.d adaemon defaults >/dev/null 2>&1 || true; fi
  if have chkconfig; then chkconfig --add adaemon >/dev/null 2>&1 || true; fi
}

write_service() {
  if systemd_running; then
    write_systemd_service
  elif have rc-service && [ -d /etc/init.d ]; then
    write_openrc_service
  elif [ -d /etc/init.d ]; then
    write_sysv_service
  else
    warn "No supported init system found; daemon can still be started manually with $BIN_DST"
  fi
}

open_firewall_port() {
  local port="$1"
  [[ "$port" =~ ^[0-9]+$ ]] || return 0
  if have firewall-cmd && firewall-cmd --state >/dev/null 2>&1; then
    firewall-cmd --permanent --add-port="$port/tcp" >/dev/null 2>&1 || true
    firewall-cmd --reload >/dev/null 2>&1 || true
  fi
  if have ufw && ufw status 2>/dev/null | grep -qi "Status: active"; then
    ufw allow "$port/tcp" >/dev/null 2>&1 || true
  fi
}

start_service() {
  if [ ! -f "$NODE_DIR/config.yml" ]; then
    warn "Skipping ADaemon start because /var/lib/node/config.yml is missing."
    return 0
  fi
  log "Starting ADaemon..."
  if systemd_running; then
    systemctl restart "$SERVICE_NAME.service" || systemctl restart "$ALIAS_SERVICE_NAME.service"
    systemctl is-active --quiet "$SERVICE_NAME.service" || systemctl is-active --quiet "$ALIAS_SERVICE_NAME.service"
  elif have rc-service; then
    rc-service adaemon restart || rc-service adaemon start
  elif have service; then
    service adaemon restart || service adaemon start
  else
    cd "$NODE_DIR"
    nohup "$BIN_DST" >/var/log/adaemon.log 2>/var/log/adaemon.err &
  fi
}

main() {
  require_root
  mkdir -p "$(dirname "$LOG_FILE")"
  touch "$LOG_FILE"
  exec > >(tee -a "$LOG_FILE") 2>&1

  log "ADPanel node daemon installation started."
  detect_platform
  install_base_packages
  install_docker
  start_docker
  install_go_toolchain
  deploy_source
  write_config
  apply_docker_compat_patch
  ensure_user
  build_binary
  ensure_user
  write_service
  open_firewall_port "$NODE_API_PORT"
  open_firewall_port "$NODE_SFTP_PORT"
  start_service
  log "ADaemon installed successfully. Config: $NODE_DIR/config.yml"
}

main "$@"
ADPANEL_NODE_INSTALLER

chmod 700 "$tmp_script"
set +e
if [ "$(id -u)" -eq 0 ]; then
  bash "$tmp_script"
else
  sudo -E bash "$tmp_script"
fi
rc=$?
set -e
rm -f "$tmp_script"
exit "$rc"
