#!/usr/bin/env bash
#
# start_worker.sh — 任务执行节点（worker）启动/停止/重启/状态/日志 脚本
#
# 用法:
#   ./scripts/start_worker.sh start              启动（默认 worker id=worker-1）
#   ./scripts/start_worker.sh start -i worker-2  指定 worker id 启动多实例
#   ./scripts/start_worker.sh stop [-i worker-2] 停止（默认/指定实例）
#   ./scripts/start_worker.sh restart
#   ./scripts/start_worker.sh status
#   ./scripts/start_worker.sh logs
#
# 可用环境变量覆盖默认值:
#   SERVER_ADDR   服务端地址    默认 http://127.0.0.1:8080
#   WORKER_ID     节点标识      默认 worker-1
#   WORKERS       并发池大小    默认 4
#   PULL_EVERY    拉取间隔      默认 1s
#   LOG_LEVEL     日志级别      默认 info
#   TOKEN         共享鉴权密钥  默认空（需与服务端一致）
#   BIN           二进制路径    默认 ./bin/worker_lightweight（缺失自动编译）
#
set -uo pipefail   # 不使用 set -e：kill/grep 的非零退出属于正常分支

# ---- 路径与变量 -------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT_DIR"

BIN="${BIN:-$ROOT_DIR/bin/worker_lightweight}"
SERVER_ADDR="${SERVER_ADDR:-http://127.0.0.1:8080}"
WORKER_ID="${WORKER_ID:-worker-1}"
WORKERS="${WORKERS:-4}"
PULL_EVERY="${PULL_EVERY:-1s}"
LOG_LEVEL="${LOG_LEVEL:-info}"
TOKEN="${TOKEN:-}"

RUN_DIR="$ROOT_DIR/run"
LOG_DIR="$ROOT_DIR/logs"
STOP_GRACE=10

# 第一个位置参数是动作子命令，其余参数解析 -i/--id
ACTION="${1:-}"
[ $# -gt 0 ] && shift
while [ $# -gt 0 ]; do
    case "$1" in
        -i|--id) WORKER_ID="${2:-}"; shift 2 ;;
        -h|--help) ACTION="help"; shift ;;
        *) printf '\033[31m[ERROR]\033[0m 未知参数: %s\n' "$1"; exit 2 ;;
    esac
done

# PID 与日志文件按 worker id 区分，支持同机多实例
PID_FILE="$RUN_DIR/worker-${WORKER_ID}.pid"
LOG_FILE="$LOG_DIR/worker-${WORKER_ID}.log"

# ---- 工具函数 ---------------------------------------------------------------
c_info()  { printf '\033[32m[INFO]\033[0m  %s\n' "$*"; }
c_warn()  { printf '\033[33m[WARN]\033[0m  %s\n' "$*"; }
c_error() { printf '\033[31m[ERROR]\033[0m %s\n' "$*"; }

usage() {
    cat <<EOF
任务执行节点管理脚本

用法: $(basename "$0") {start|stop|restart|status|logs} [-i worker-id]

环境变量（可选）:
  SERVER_ADDR=$SERVER_ADDR
  WORKER_ID=$WORKER_ID
  WORKERS=$WORKERS
  PULL_EVERY=$PULL_EVERY
  LOG_LEVEL=$LOG_LEVEL
  TOKEN=${TOKEN:-<empty=no-auth>}
  BIN=$BIN
EOF
}

ensure_dirs() {
    mkdir -p "$RUN_DIR" "$LOG_DIR" "$(dirname "$BIN")"
}

is_running() {
    [ -f "$PID_FILE" ] || return 1
    local pid
    pid="$(cat "$PID_FILE" 2>/dev/null)"
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null
}

ensure_bin() {
    local need_build=0
    if [ ! -x "$BIN" ]; then
        need_build=1
    else
        if find "$ROOT_DIR" -name '*.go' -newer "$BIN" -print -quit | grep -q .; then
            need_build=1
        fi
    fi
    if [ "$need_build" -eq 1 ]; then
        c_info "编译二进制: $BIN"
        if ! (cd "$ROOT_DIR" && go build -o "$BIN" .); then
            c_error "编译失败，请确认已安装 Go 并执行过 go mod tidy"
            return 1
        fi
        c_info "编译完成"
    fi
}

# worker 无监听端口，启动前探测一下 server 是否可达，避免无意义重连刷屏
check_server_reachable() {
    if command -v curl >/dev/null 2>&1; then
        if ! curl -fsS -o /dev/null --max-time 2 "$SERVER_ADDR/api/tasks" 2>/dev/null; then
            c_warn "无法连接服务端 $SERVER_ADDR，worker 仍会启动并重试，请确认服务端已运行"
        else
            c_info "服务端可达: $SERVER_ADDR"
        fi
    fi
}

# ---- 生命周期命令 -----------------------------------------------------------
do_start() {
    ensure_dirs
    if is_running; then
        c_warn "worker[$WORKER_ID] 已在运行 (PID=$(cat "$PID_FILE"))"
        return 0
    fi
    ensure_bin || return 1
    check_server_reachable
    rm -f "$PID_FILE"

    c_info "启动 worker: id=$WORKER_ID workers=$WORKERS server=$SERVER_ADDR auth=$([ -n "$TOKEN" ] && echo yes || echo no)"
    nohup "$BIN" worker \
        --server "$SERVER_ADDR" \
        --id "$WORKER_ID" \
        --workers "$WORKERS" \
        --pull-every "$PULL_EVERY" \
        --log-level "$LOG_LEVEL" \
        ${TOKEN:+--token "$TOKEN"} \
        >> "$LOG_FILE" 2>&1 &
    local pid=$!
    echo "$pid" > "$PID_FILE"
    sleep 1
    if is_running; then
        c_info "已启动 PID=$pid，日志: $LOG_FILE"
    else
        c_error "进程启动后立即退出，请查看日志: $LOG_FILE"
        rm -f "$PID_FILE"
        return 1
    fi
}

do_stop() {
    if ! is_running; then
        c_warn "worker[$WORKER_ID] 未运行"
        rm -f "$PID_FILE"
        return 0
    fi
    local pid
    pid="$(cat "$PID_FILE")"
    c_info "发送 SIGTERM 优雅停止 worker[$WORKER_ID] PID=$pid ..."
    kill -TERM "$pid" 2>/dev/null || true

    # 等待 worker 池 drain 在途任务后退出
    local waited=0
    while kill -0 "$pid" 2>/dev/null; do
        if [ "$waited" -ge "$STOP_GRACE" ]; then
            c_warn "等待 ${STOP_GRACE}s 未退出，发送 SIGKILL 强杀"
            kill -KILL "$pid" 2>/dev/null || true
            sleep 1
            break
        fi
        sleep 1
        waited=$((waited + 1))
    done
    rm -f "$PID_FILE"
    c_info "worker[$WORKER_ID] 已停止"
}

do_status() {
    if is_running; then
        c_info "worker[$WORKER_ID] 运行中 PID=$(cat "$PID_FILE")"
        return 0
    fi
    c_warn "worker[$WORKER_ID] 未运行"
    return 1
}

do_logs() {
    [ -f "$LOG_FILE" ] || { c_warn "日志文件不存在: $LOG_FILE"; return 1; }
    exec tail -n 100 -f "$LOG_FILE"
}

# ---- 入口 -------------------------------------------------------------------
case "$ACTION" in
    start)   do_start ;;
    stop)    do_stop ;;
    restart) do_stop; do_start ;;
    status)  do_status ;;
    logs)    do_logs ;;
    help|-h|--help) usage ;;
    "")
        usage
        exit 2
        ;;
    *)
        c_error "未知命令: $ACTION"
        usage
        exit 2
        ;;
esac
