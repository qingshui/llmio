#!/bin/bash
# scripts/build.sh — 编译 LLMIO（前端 + 后端，产出单二进制）
#
# 用法:
#   scripts/build.sh           # 默认：构建前端 + CGO_ENABLED=0 静态后端
#   scripts/build.sh --skip-webui   # 跳过前端，只重编后端（dist 已存在时）
#   scripts/build.sh --cgo      # 用 CGO 启用方式编译（非静态，依赖 libc）
#
# 产物: ./llmio（已嵌入 webui/dist）

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WEBUI_DIR="$ROOT/webui"
DIST_DIR="$WEBUI_DIR/dist"
BIN="$ROOT/llmio"
VERSION="${VERSION:-dev}"

USE_CGO=0
SKIP_WEBUI=0
for arg in "$@"; do
  case "$arg" in
    --skip-webui) SKIP_WEBUI=1 ;;
    --cgo)        USE_CGO=1 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

# ---------- 前端 ----------
if [ "$SKIP_WEBUI" -eq 0 ]; then
  if [ ! -d "$WEBUI_DIR/node_modules" ]; then
    echo "[build] 安装前端依赖 (pnpm install || true)..."
    ( cd "$WEBUI_DIR" && pnpm install || true )
  fi
  echo "[build] 构建前端 (vite build) -> $DIST_DIR"
  ( cd "$WEBUI_DIR" && node_modules/.bin/tsc -b && node_modules/.bin/vite build )
else
  if [ ! -f "$DIST_DIR/index.html" ]; then
    echo "[build] 错误：--skip-webui 但 $DIST_DIR/index.html 不存在，请先构建前端" >&2
    exit 1
  fi
  echo "[build] 跳过前端，使用已有 $DIST_DIR"
fi

# ---------- 后端 ----------
LDFLAGS="-s -w -X github.com/atopos31/llmio/consts.Version=${VERSION}"
if [ "$USE_CGO" -eq 0 ]; then
  CGO_FLAGS="CGO_ENABLED=0"
  EXTRA="-trimpath"
  echo "[build] 编译后端 (静态, CGO disabled) -> $BIN"
else
  CGO_FLAGS="CGO_ENABLED=1"
  EXTRA=""
  echo "[build] 编译后端 (CGO enabled) -> $BIN"
fi

env $CGO_FLAGS go build $EXTRA -ldflags="$LDFLAGS" -o "$BIN" .

echo "[build] 完成: $BIN ($(ls -lh "$BIN" | awk '{print $5}'))"
