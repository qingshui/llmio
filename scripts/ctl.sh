#!/bin/bash
# scripts/ctl.sh — LLMIO 服务启停管理
#
# 用法:
#   scripts/ctl.sh start    启动（后台，日志写入 logs/llmio.log）
#   scripts/ctl.sh stop     停止
#   scripts/ctl.sh restart  重启
#   scripts/ctl.sh status   查看状态
#   scripts/ctl.sh logs     跟踪日志 (tail -f)
#
# 环境变量（可在 .env 或 shell 中覆盖）:
#   TOKEN            必填，API/控制台鉴权 token
#   LLMIO_SERVER_PORT 监听端口，默认 8070
#   GIN_MODE         release/debug，默认 release
#   TZ               时区，默认 Asia/Shanghai

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

APP="llmio"
BIN="$ROOT/$APP"
PID_FILE="$ROOT/run/llmio.pid"
LOG_DIR="$ROOT/logs"
LOG_FILE="$LOG_DIR/llmio.log"

# ---------- 配置加载 ----------
# 优先级：shell 环境变量 > .env 文件 > 脚本默认值
# TOKEN 为敏感信息，禁止硬编码到仓库，必须从环境或 .env 读取。
ENV_FILE="${ENV_FILE:-$ROOT/.env}"
if [ -f "$ENV_FILE" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi
: "${TOKEN:?TOKEN 未配置：请在 .env 文件或环境变量中设置（.env 已被 .gitignore 忽略）}"
LLMIO_SERVER_PORT="${LLMIO_SERVER_PORT:-8070}"
GIN_MODE="${GIN_MODE:-release}"
TZ="${TZ:-Asia/Shanghai}"

mkdir -p "$LOG_DIR" "$(dirname "$PID_FILE")"

is_running() {
  [ -f "$PID_FILE" ] || return 1
  local pid; pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null
}

start() {
  if is_running; then
    echo "[ctl] 已在运行 (PID $(cat "$PID_FILE"))"
    return 0
  fi
  if [ ! -x "$BIN" ]; then
    echo "[ctl] 错误：二进制 $BIN 不存在或不可执行，请先运行 scripts/build.sh" >&2
    exit 1
  fi

  # 日志轮转：保留最近 5 份
  if [ -f "$LOG_FILE" ]; then
    for i in 4 3 2 1; do
      [ -f "$LOG_FILE.$i" ] && mv "$LOG_FILE.$i" "$LOG_FILE.$((i+1))"
    done
    mv "$LOG_FILE" "$LOG_FILE.1" 2>/dev/null || true
  fi

  echo "[ctl] 启动 $APP (端口 $LLMIO_SERVER_PORT)，日志 -> $LOG_FILE"
  # setsid 让进程脱离当前会话，父进程退出后仍存活
  setsid env \
    TOKEN="$TOKEN" \
    LLMIO_SERVER_PORT="$LLMIO_SERVER_PORT" \
    GIN_MODE="$GIN_MODE" \
    TZ="$TZ" \
    "$BIN" > "$LOG_FILE" 2>&1 < /dev/null &
  local pid=$!
  echo "$pid" > "$PID_FILE"
  disown "$pid" 2>/dev/null || true

  # 等待监听就绪
  local waited=0
  while [ "$waited" -lt 10 ]; do
    if kill -0 "$pid" 2>/dev/null; then
      if (ss -ltn 2>/dev/null || netstat -ltn 2>/dev/null) | grep -q ":$LLMIO_SERVER_PORT "; then
        echo "[ctl] 已就绪 (PID $pid)"
        return 0
      fi
    else
      echo "[ctl] 错误：进程启动后立即退出，日志：" >&2
      tail -20 "$LOG_FILE" >&2 || true
      rm -f "$PID_FILE"
      exit 1
    fi
    sleep 1; waited=$((waited+1))
  done
  echo "[ctl] 已启动但端口未在 10s 内就绪，查看日志: $LOG_FILE (PID $pid)"
}

stop() {
  if ! is_running; then
    echo "[ctl] 未运行"
    rm -f "$PID_FILE"
    return 0
  fi
  local pid; pid="$(cat "$PID_FILE")"
  echo "[ctl] 停止 (PID $pid)..."
  kill "$pid" 2>/dev/null || true
  local waited=0
  while [ "$waited" -lt 10 ] && kill -0 "$pid" 2>/dev/null; do
    sleep 1; waited=$((waited+1))
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "[ctl] 未能优雅退出，强制终止"
    kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$PID_FILE"
  echo "[ctl] 已停止"
}

status() {
  if is_running; then
    local pid; pid="$(cat "$PID_FILE")"
    echo "[ctl] 运行中 (PID $pid)，端口 $LLMIO_SERVER_PORT"
    if (ss -ltn 2>/dev/null || netstat -ltn 2>/dev/null) | grep -q ":$LLMIO_SERVER_PORT "; then
      echo "[ctl] 端口监听正常"
    fi
  else
    echo "[ctl] 未运行"
  fi
}

logs() {
  if [ ! -f "$LOG_FILE" ]; then
    echo "[ctl] 日志文件不存在: $LOG_FILE"
    exit 1
  fi
  echo "[ctl] 跟踪日志 $LOG_FILE (Ctrl+C 退出)"
  tail -f "$LOG_FILE"
}

case "${1:-}" in
  start)   start ;;
  stop)    stop ;;
  restart) stop; start ;;
  status)  status ;;
  logs)    logs ;;
  *)
    cat <<EOF
用法: scripts/ctl.sh {start|stop|restart|status|logs}
EOF
    exit 2 ;;
esac
