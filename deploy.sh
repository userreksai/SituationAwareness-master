#!/usr/bin/env bash
# SituationAwareness Master 一键更新、编译和启动脚本（systemd）。
#
# 首次运行：
#   1. chmod +x deploy.sh
#   2. sudo ./deploy.sh
#   3. 如果脚本提示新建了 .env，修改其中的 AGENT_SHARED_TOKEN 后再次运行。
#
# 后续更新：
#   sudo ./deploy.sh
#
# 可覆盖的参数示例：
#   sudo BRANCH=main HEALTH_URL=http://127.0.0.1:8001/healthz ./deploy.sh

set -Eeuo pipefail
IFS=$'\n\t'

APP_NAME="${APP_NAME:-situation-awareness-master}"
SERVICE_NAME="${SERVICE_NAME:-${APP_NAME}}"
REPO_URL="${REPO_URL:-https://github.com/userreksai/SituationAwareness-master.git}"
BRANCH="${BRANCH:-main}"
SOURCE_DIR="${SOURCE_DIR:-/usr/local/SituationAwareness-master}"
INSTALL_DIR="${INSTALL_DIR:-/opt/${APP_NAME}}"
ENV_SOURCE="${ENV_SOURCE:-${SOURCE_DIR}/.env}"
ENV_TARGET="${ENV_TARGET:-/etc/default/${APP_NAME}}"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8001/healthz}"
HEALTH_RETRIES="${HEALTH_RETRIES:-30}"
RUN_TESTS="${RUN_TESTS:-true}"
GO_INSTALL_ROOT="${GO_INSTALL_ROOT:-/opt/go-toolchains}"

BUILD_DIR=""
GO_BIN=""

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

die() {
  log "错误：$*" >&2
  exit 1
}

cleanup() {
  if [[ -n "${BUILD_DIR}" && -d "${BUILD_DIR}" ]]; then
    rm -rf -- "${BUILD_DIR}"
  fi
}
trap cleanup EXIT
trap 'die "第 ${LINENO} 行执行失败"' ERR

require_root() {
  [[ "${EUID}" -eq 0 ]] || die "请使用 root 运行：sudo $0"
  [[ "$(uname -s)" == "Linux" ]] || die "此脚本仅支持 Linux"
  command -v systemctl >/dev/null 2>&1 || die "当前系统没有 systemd/systemctl"
}

install_prerequisites() {
  local missing=()
  local command_name
  for command_name in git curl tar sha256sum install; do
    command -v "${command_name}" >/dev/null 2>&1 || missing+=("${command_name}")
  done
  if ((${#missing[@]} == 0)); then
    return
  fi

  command -v apt-get >/dev/null 2>&1 || \
    die "缺少命令：${missing[*]}；请先安装 git、curl、ca-certificates、tar、coreutils"
  log "安装部署依赖：${missing[*]}"
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y git curl ca-certificates tar coreutils
}

lock_deployment() {
  if command -v flock >/dev/null 2>&1; then
    install -d -m 0755 /run/lock
    exec 9>"/run/lock/${APP_NAME}-deploy.lock"
    flock -n 9 || die "另一个部署任务正在执行"
  fi
}

backup_untracked_remote_collisions() {
  local remote_ref="origin/${BRANCH}"
  local path
  local backup_dir=""

  while IFS= read -r -d '' path; do
    if ! git -C "${SOURCE_DIR}" cat-file -e "${remote_ref}:${path}" 2>/dev/null; then
      continue
    fi
    if [[ -z "${backup_dir}" ]]; then
      backup_dir="$(mktemp -d "${TMPDIR:-/tmp}/${APP_NAME}.untracked.XXXXXX")"
    fi
    mkdir -p -- "$(dirname -- "${backup_dir}/${path}")"
    mv -- "${SOURCE_DIR}/${path}" "${backup_dir}/${path}"
    log "已备份会阻塞更新的未跟踪文件：${path}"
  done < <(git -C "${SOURCE_DIR}" ls-files --others --exclude-standard -z)

  if [[ -n "${backup_dir}" ]]; then
    log "未跟踪文件备份目录：${backup_dir}"
  fi
}

update_source() {
  if [[ ! -d "${SOURCE_DIR}/.git" ]]; then
    if [[ -e "${SOURCE_DIR}" ]] && [[ -n "$(find "${SOURCE_DIR}" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" ]]; then
      die "源码目录不是 Git 仓库且不为空：${SOURCE_DIR}"
    fi
    log "克隆 ${REPO_URL} (${BRANCH})"
    install -d -m 0755 "$(dirname "${SOURCE_DIR}")"
    git clone --branch "${BRANCH}" --single-branch "${REPO_URL}" "${SOURCE_DIR}"
    return
  fi

  if [[ -n "$(git -C "${SOURCE_DIR}" status --porcelain --untracked-files=no)" ]]; then
    die "仓库中存在未提交的受版本控制文件修改；为避免覆盖数据，已停止更新"
  fi

  log "拉取 origin/${BRANCH}"
  git -C "${SOURCE_DIR}" fetch --prune origin "${BRANCH}"
  backup_untracked_remote_collisions
  git -C "${SOURCE_DIR}" checkout "${BRANCH}"
  git -C "${SOURCE_DIR}" merge --ff-only "origin/${BRANCH}"
  log "当前版本：$(git -C "${SOURCE_DIR}" rev-parse --short HEAD)"
}

prepare_environment() {
  if [[ ! -f "${ENV_SOURCE}" ]]; then
    [[ -f "${SOURCE_DIR}/.env.example" ]] || die "缺少 ${ENV_SOURCE} 和 .env.example"
    install -m 0600 "${SOURCE_DIR}/.env.example" "${ENV_SOURCE}"
    log "已创建配置文件：${ENV_SOURCE}"
    die "请先修改 AGENT_SHARED_TOKEN 等配置，然后重新运行本脚本"
  fi

  if grep -Eq '^AGENT_SHARED_TOKEN=replace-with-the-same-token-used-by-agents([[:space:]]*)$' "${ENV_SOURCE}"; then
    die "${ENV_SOURCE} 仍在使用示例 AGENT_SHARED_TOKEN，请改成与 Agent 相同的令牌"
  fi
  if ! grep -Eq '^AGENT_SHARED_TOKEN=.+$' "${ENV_SOURCE}"; then
    log "警告：AGENT_SHARED_TOKEN 为空，Master 调用 Agent 时不会携带认证令牌"
  fi

  install -d -m 0755 "$(dirname "${ENV_TARGET}")"
  install -m 0600 "${ENV_SOURCE}" "${ENV_TARGET}"
}

go_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    *) die "不支持的 CPU 架构：$(uname -m)" ;;
  esac
}

install_go_toolchain() {
  local version filename archive metadata expected_checksum actual_checksum toolchain_dir temp_dir arch
  arch="$(go_arch)"

  log "获取 Go 官方稳定版本信息"
  version="$(curl --fail --silent --show-error --location 'https://go.dev/VERSION?m=text' | sed -n '1{s/^go//;p;}')"
  [[ "${version}" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || die "无法识别 Go 版本：${version}"

  toolchain_dir="${GO_INSTALL_ROOT}/go${version}"
  if [[ -x "${toolchain_dir}/bin/go" ]]; then
    GO_BIN="${toolchain_dir}/bin/go"
    log "复用 Go $(${GO_BIN} version)"
    return
  fi

  BUILD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/${APP_NAME}.deploy.XXXXXX")"
  filename="go${version}.linux-${arch}.tar.gz"
  archive="${BUILD_DIR}/${filename}"
  metadata="${BUILD_DIR}/go-downloads.json"
  log "下载 Go ${version} linux/${arch}"
  curl --fail --silent --show-error --location \
    "https://go.dev/dl/${filename}" -o "${archive}"
  curl --fail --silent --show-error --location \
    'https://go.dev/dl/?mode=json' -o "${metadata}"
  expected_checksum="$(awk -v target="${filename}" '
    index($0, "\"filename\": \"" target "\"") { found = 1; next }
    found && /\"sha256\":/ {
      gsub(/.*\"sha256\": \"/, "")
      gsub(/\".*/, "")
      print
      exit
    }
  ' "${metadata}")"
  actual_checksum="$(sha256sum "${archive}" | awk '{print $1}')"
  [[ "${expected_checksum}" =~ ^[0-9a-f]{64}$ ]] || die "无法从 Go 官方元数据读取 SHA-256"
  [[ "${actual_checksum}" == "${expected_checksum}" ]] || \
    die "Go 安装包 SHA-256 校验失败"

  install -d -m 0755 "${GO_INSTALL_ROOT}"
  temp_dir="${GO_INSTALL_ROOT}/.go${version}.tmp.$$"
  install -d -m 0755 "${temp_dir}"
  tar -xzf "${archive}" -C "${temp_dir}" --strip-components=1
  mv "${temp_dir}" "${toolchain_dir}"
  GO_BIN="${toolchain_dir}/bin/go"
  log "已安装 $(${GO_BIN} version) 到 ${toolchain_dir}"
}

build_application() {
  local build_arch
  build_arch="$(go_arch)"
  if [[ -z "${BUILD_DIR}" ]]; then
    BUILD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/${APP_NAME}.deploy.XXXXXX")"
  fi

  if [[ "${RUN_TESTS}" == "true" ]]; then
    log "运行测试"
    (cd "${SOURCE_DIR}" && GOTOOLCHAIN=local "${GO_BIN}" test ./...)
  fi

  log "编译 ${APP_NAME}"
  (
    cd "${SOURCE_DIR}"
    CGO_ENABLED=0 GOOS=linux GOARCH="${build_arch}" GOTOOLCHAIN=local \
      "${GO_BIN}" build -trimpath -ldflags='-s -w' \
      -o "${BUILD_DIR}/${APP_NAME}" ./cmd/master
  )
  [[ -x "${BUILD_DIR}/${APP_NAME}" ]] || die "编译完成后未找到可执行文件"
}

ensure_service_user() {
  if ! getent group "${APP_NAME}" >/dev/null 2>&1; then
    groupadd --system "${APP_NAME}"
  fi
  if ! id "${APP_NAME}" >/dev/null 2>&1; then
    useradd --system --gid "${APP_NAME}" --home-dir "${INSTALL_DIR}" \
      --shell /usr/sbin/nologin "${APP_NAME}"
  fi
}

write_service_file() {
  local unit_file="${BUILD_DIR}/${SERVICE_NAME}.service"
  cat >"${unit_file}" <<EOF
[Unit]
Description=SituationAwareness Master
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=${APP_NAME}
Group=${APP_NAME}
WorkingDirectory=${INSTALL_DIR}
EnvironmentFile=${ENV_TARGET}
ExecStart=${INSTALL_DIR}/${APP_NAME}
Restart=on-failure
RestartSec=3s
TimeoutStopSec=15s
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
CapabilityBoundingSet=
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

[Install]
WantedBy=multi-user.target
EOF
  install -m 0644 "${unit_file}" "${SERVICE_FILE}"
}

health_check() {
  local attempt
  for ((attempt = 1; attempt <= HEALTH_RETRIES; attempt++)); do
    if curl --fail --silent --show-error --max-time 2 "${HEALTH_URL}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

install_and_restart() {
  local binary="${INSTALL_DIR}/${APP_NAME}"
  local backup="${BUILD_DIR}/${APP_NAME}.previous"
  local had_previous=false

  install -d -o "${APP_NAME}" -g "${APP_NAME}" -m 0755 "${INSTALL_DIR}"
  if [[ -f "${binary}" ]]; then
    cp -p "${binary}" "${backup}"
    had_previous=true
  fi

  install -o root -g root -m 0755 "${BUILD_DIR}/${APP_NAME}" "${binary}"
  write_service_file
  systemctl daemon-reload
  systemctl enable "${SERVICE_NAME}.service" >/dev/null
  systemctl restart "${SERVICE_NAME}.service"

  log "等待健康检查：${HEALTH_URL}"
  if health_check; then
    log "部署成功：${SERVICE_NAME}.service 已启动"
    systemctl --no-pager --full status "${SERVICE_NAME}.service" | sed -n '1,12p'
    return
  fi

  log "新版本健康检查失败，开始回滚"
  systemctl --no-pager --full status "${SERVICE_NAME}.service" || true
  journalctl -u "${SERVICE_NAME}.service" -n 50 --no-pager || true
  if [[ "${had_previous}" == "true" ]]; then
    install -o root -g root -m 0755 "${backup}" "${binary}"
    systemctl restart "${SERVICE_NAME}.service"
    if health_check; then
      die "新版本启动失败，已恢复并启动上一版本"
    fi
    die "新版本启动失败，而且上一版本也未通过健康检查"
  fi
  systemctl stop "${SERVICE_NAME}.service" || true
  die "首次部署启动失败，服务已停止；请查看上方日志"
}

main() {
  require_root
  install_prerequisites
  lock_deployment
  update_source
  prepare_environment
  install_go_toolchain
  build_application
  ensure_service_user
  install_and_restart
  log "查看日志：journalctl -u ${SERVICE_NAME} -f"
}

main "$@"
