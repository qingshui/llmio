#!/bin/bash
# scripts/setup-env.sh — LLMIO 一键开发环境部署（用户级，无 root）
#
# 在当前用户目录下安装并配置:
#   - Go toolchain (默认 1.26.1，匹配 go.mod)
#   - pnpm (通过 npm 全局安装到用户目录)
#   - GOPATH / GOPROXY / GOSUMDB
#   - 前端依赖 (pnpm install + native rebuild)
#   - 首次产物: webui/dist + ./llmio 二进制
#
# 用法:
#   scripts/setup-env.sh                # 默认安装 Go 1.26.1
#   scripts/setup-env.sh go1.25.3       # 指定 Go 版本
#   scripts/setup-env.sh --no-build     # 只装工具链，不构建
#   scripts/setup-env.sh --skip-webui   # 构建时跳过前端
#
# 选项可组合，第一个非 -- 参数视为 Go 版本号。
#
# 网络: 脚本会自动用百度内网代理拉取外网资源 (go.dev / npmjs.org)。
# 已装的 Go/pnpm 会被复用，不会重复安装。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# ---------------- 可调参数 ----------------
GO_VERSION_DEFAULT="1.26.1"
GO_INSTALL_DIR="${HOME}/.local/go-1.26.1"
NPM_GLOBAL_DIR="${HOME}/.local/npm-global"
GOPATH_DIR="${HOME}/go"

# 代理: GitHub / 外网走 agent.baidu.com，npm/pypi 走 10.99.77.5
PROXY_FOREIGN="http://agent.baidu.com:8891"
PROXY_GENERAL="http://10.99.77.5:8891"
NO_PROXY=".bcebos.com,pip.baidu.com,.baidubce.com,.baidu-int.com,pypi.baidu.com,localhost,127.0.0.1"

# ---------------- 解析参数 ----------------
GO_VERSION=""
DO_BUILD=1
SKIP_WEBUI=0
for arg in "$@"; do
  case "$arg" in
    --no-build)    DO_BUILD=0 ;;
    --skip-webui)  SKIP_WEBUI=1 ;;
    --help|-h)
      sed -n '2,20p' "$0"; exit 0 ;;
    *)
      if [ -z "$GO_VERSION" ]; then GO_VERSION="$arg"
      else echo "[setup] 未知参数: $arg" >&2; exit 2
      fi
      ;;
  esac
done
[ -z "$GO_VERSION" ] && GO_VERSION="$GO_VERSION_DEFAULT"

# 根据版本号重算安装目录（允许覆盖默认）
if [ "$GO_VERSION" != "$GO_VERSION_DEFAULT" ]; then
  GO_INSTALL_DIR="${HOME}/.local/go-${GO_VERSION}"
fi

# ---------------- 颜色/日志 ----------------
C_G=$'\033[32m'; C_Y=$'\033[33m'; C_R=$'\033[31m'; C_0=$'\033[0m'
log()  { echo "${C_G}[setup]${C_0} $*"; }
warn() { echo "${C_Y}[setup]${C_0} $*" >&2; }
err()  { echo "${C_R}[setup]${C_0} $*" >&2; }

# ---------------- 1. Go toolchain ----------------
install_go() {
  if [ -x "${GO_INSTALL_DIR}/bin/go" ]; then
    log "Go 已存在: ${GO_INSTALL_DIR}/bin/go ($(${GO_INSTALL_DIR}/bin/go version 2>/dev/null | awk '{print $2,$3}'))"
    return 0
  fi

  local arch os
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) err "不支持的架构: $arch"; exit 3 ;;
  esac
  os="$(uname -s | tr A-Z a-z)"

  local pkg="go${GO_VERSION}.${os}-${arch}.tar.gz"
  local url="https://go.dev/dl/${pkg}"
  local tmp; tmp="$(mktemp -t llmio-go.XXXXXX.tar.gz)"

  log "下载 Go ${GO_VERSION}: ${url}"
  http_proxy="$PROXY_FOREIGN" https_proxy="$PROXY_FOREIGN" no_proxy="$NO_PROXY" \
    curl -fL --max-time 600 -o "$tmp" "$url"

  mkdir -p "$(dirname "$GO_INSTALL_DIR")"
  rm -rf "$GO_INSTALL_DIR"
  tar -xzf "$tmp" -C "$(dirname "$GO_INSTALL_DIR")"
  mv "$(dirname "$GO_INSTALL_DIR")/go" "$GO_INSTALL_DIR"
  rm -f "$tmp"

  log "Go 安装完成: ${GO_INSTALL_DIR}/bin/go"
  "${GO_INSTALL_DIR}/bin/go" version
}

# ---------------- 2. pnpm ----------------
install_pnpm() {
  # 优先识别用户级安装目录里的 pnpm，避免 PATH 未刷新导致误判重装
  local pnpm_bin=""
  if [ -x "${NPM_GLOBAL_DIR}/bin/pnpm" ]; then
    pnpm_bin="${NPM_GLOBAL_DIR}/bin/pnpm"
  elif command -v pnpm >/dev/null 2>&1; then
    pnpm_bin="$(command -v pnpm)"
  fi
  if [ -n "$pnpm_bin" ] && [ "$("$pnpm_bin" -v 2>/dev/null | head -c1)" != "" ]; then
    log "pnpm 已存在: $pnpm_bin ($("$pnpm_bin" -v))"
    # 确保当前会话 PATH 可见
    case ":${PATH}:" in *":${NPM_GLOBAL_DIR}/bin:"*) ;; *) export PATH="${NPM_GLOBAL_DIR}/bin:${PATH}";; esac
    return 0
  fi

  if ! command -v npm >/dev/null 2>&1; then
    err "npm 不存在，请先安装 Node.js"; exit 4
  fi

  mkdir -p "$NPM_GLOBAL_DIR"
  npm config set prefix "$NPM_GLOBAL_DIR"

  log "通过 npm 安装 pnpm 到 $NPM_GLOBAL_DIR"
  http_proxy="$PROXY_GENERAL" https_proxy="$PROXY_GENERAL" no_proxy="$NO_PROXY" \
    npm install -g pnpm >/dev/null

  export PATH="${NPM_GLOBAL_DIR}/bin:${PATH}"
  pnpm_bin="${NPM_GLOBAL_DIR}/bin/pnpm"
  log "pnpm 安装完成: $("$pnpm_bin" -v)"
}

# ---------------- 3. 写入 ~/.bashrc ----------------
ensure_bashrc() {
  local marker="# === llmio dev toolchain (user-local, no root) ==="
  if grep -qF "$marker" "$HOME/.bashrc" 2>/dev/null; then
    log ".bashrc 已配置 toolchain 段落，跳过"
    return 0
  fi

  log "写入 toolchain 环境到 ~/.bashrc"
  cat >> "$HOME/.bashrc" <<EOF

$marker
export PATH="\$HOME/.local/go-${GO_VERSION}/bin:\$HOME/.local/npm-global/bin:\$PATH"
export GOPATH="\$HOME/go"
export GOPROXY="https://goproxy.cn,direct"
export GOSUMDB=off
# === end llmio dev toolchain ===
EOF
}

# ---------------- 4. 前端依赖 ----------------
install_webui_deps() {
  [ -f "$ROOT/webui/package.json" ] || { warn "未找到 webui/package.json，跳过前端"; return 0; }

  if [ -d "$ROOT/webui/node_modules/.pnpm" ]; then
    log "前端依赖已安装，跳过 pnpm install"
  else
    log "安装前端依赖 (pnpm install)"
    ( cd "$ROOT/webui" \
      && http_proxy="$PROXY_GENERAL" https_proxy="$PROXY_GENERAL" no_proxy="$NO_PROXY" \
         pnpm install )
  fi

  # vite build 依赖的 native binding，pnpm 默认会忽略 build 脚本，强制 rebuild
  log "rebuild native 依赖 (esbuild/@swc/core/@tailwindcss/oxide)"
  ( cd "$ROOT/webui" \
    && pnpm rebuild esbuild @swc/core @tailwindcss/oxide >/dev/null 2>&1 || true )

  # vite 7 在 build 前会做 deps 状态检查，pnpm 默认忽略 native build 脚本会触发
  # ERR_PNPM_IGNORED_BUILDS 导致 build 失败。确保 pnpm-workspace.yaml 白名单存在（幂等）。
  local ws="$ROOT/webui/pnpm-workspace.yaml"
  if ! grep -q 'onlyBuiltDependencies' "$ws" 2>/dev/null; then
    log "写入 pnpm-workspace.yaml 白名单"
    cat > "$ws" <<'YAML'
onlyBuiltDependencies:
  - "@swc/core"
  - "@tailwindcss/oxide"
  - esbuild
YAML
  fi
}

# ---------------- 5. 构建 ----------------
do_build() {
  log "执行构建 (scripts/build.sh)"
  if [ "$SKIP_WEBUI" -eq 1 ]; then
    bash "$ROOT/scripts/build.sh" --skip-webui
  else
    bash "$ROOT/scripts/build.sh"
  fi
}

# ---------------- 主流程 ----------------
main() {
  log "ROOT=$ROOT  HOME=$HOME"
  install_go
  install_pnpm
  ensure_bashrc

  # 当前会话生效 PATH
  export PATH="${GO_INSTALL_DIR}/bin:${NPM_GLOBAL_DIR}/bin:${PATH}"
  export GOPATH="$GOPATH_DIR" GOPROXY="https://goproxy.cn,direct" GOSUMDB=off

  install_webui_deps

  if [ "$DO_BUILD" -eq 1 ]; then
    do_build
  fi

  echo
  log "完成。"
  echo "  Go    : ${GO_INSTALL_DIR}/bin/go ($(${GO_INSTALL_DIR}/bin/go version | awk '{print $3}'))"
  echo "  pnpm  : $(command -v pnpm) ($(pnpm -v))"
  echo "  GOPATH: $GOPATH_DIR"
  if [ "$DO_BUILD" -eq 1 ]; then
    echo "  二进制 : $ROOT/llmio ($(ls -lh "$ROOT/llmio" 2>/dev/null | awk '{print $5}'))"
  fi
  echo
  echo "  新开终端请先:  source ~/.bashrc"
  echo "  启动服务:      TOKEN=xxx scripts/ctl.sh start"
}

main "$@"
