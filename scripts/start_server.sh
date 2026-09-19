#!/usr/bin/env bash
#
# start_server.sh — 调度服务端 启动/停止/重启/状态/日志 脚本
#
# 用法:
#   ./scripts/start_server.sh start     启动（后台运行，写 PID）
#   ./scripts/start_server.sh stop      优雅停止（SIGTERM，超时强杀）
#   ./scripts/start_server.sh restart   重启
#   ./scripts/start_server.sh status    查看进程与 HTTP 健康状态
#   ./scripts/start_server.sh logs      跟踪日志（tail -f）
#   ./scripts/start_server.sh -h        帮助
#
# 可用环境变量覆盖默认值:
#   SERVER_ADDR    监听地址      默认 127.0.0.1:8080
#   DB_PATH        SQLite 路径   默认 ./data/tasks.db
#   SYNC_EVERY     cron 同步间隔 默认 30s
#   LOG_LEVEL      日志级别      默认 info
#   TOKEN          共享鉴权密钥  默认空（不启用鉴权）
#   TLS_CERT       TLS 证书路径  默认空（不启用 HTTPS）
#   TLS_KEY        TLS 私钥路径  默认空（不启用 HTTPS）
#   AUDIT_LOG      审计日志路径  默认空（不启用审计）
#   RATE_LIMIT     每分钟每IP请求上限 默认 0（不限）
#   BIN            二进制路径    默认 ./bin/worker_lightweight（缺失自动编译）
#
# TLS 快速启用示例 (使用自签名证书):
#   openssl req -x509 -newkey rsa:2048 -keyout key.pem -out cert.pem -days 365 -nodes
#   TLS_CERT=cert.pem TLS_KEY=key.pem SERVER_ADDR=0.0.0.0:8443 ./scripts/start_server.sh start
#
# 完整安全加固示例:
#   TOKEN=secret AUDIT_LOG=./logs/audit.log RATE_LIMIT=120 \
#   TLS_CERT=cert.pem TLS_KEY=key.pem SERVER_ADDR=0.0.0.0:8443 \
#   ./scripts/start_server.sh start
#
set -uo pipefail   # 注意：不使用 set -e，kill/grep 等命令的非零退出是正常分支

# ---- 路径与变量 -------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT_DIR"

BIN="${BIN:-$ROOT_DIR/bin/worker_lightweight}"
SERVER_ADDR="${SERVER_ADDR:-127.0.0.1:8080}"
DB_PATH="${DB_PATH:-$ROOT_DIR/data/tasks.db}"
SYNC_EVERY="${SYNC_EVERY:-30s}"
LOG_LEVEL="${LOG_LEVEL:-info}"
TOKEN="${TOKEN:-}"
TLS_CERT="${TLS_CERT:-}"
TLS_KEY="${TLS_KEY:-}"
AUDIT_LOG="${AUDIT_LOG:-}"
RATE_LIMIT="${RATE_LIMIT:-0}"

RUN_DIR="$ROOT_DIR/run"
LOG_DIR="$ROOT_DIR/logs"
PID_FILE="$RUN_DIR/server.pid"
LOG_FILE="$LOG_DIR/server.log"

# 健康检查等待参数
HEALTH_RETRIES=15          # 最多探测次数
HEALTH_INTERVAL=1          # 每次间隔秒数
STOP_GRACE=10              # 优雅停止等待秒数，超时后 SIGKILL

# ---- 工具函数 ---------------------------------------------------------------
c_info()  { printf '\033[32m[INFO]\033[0m  %s\n' "$*"; }
c_warn()  { printf '\033[33m[WARN]\033[0m  %s\n' "$*"; }
c_error() { printf '\033[31m[ERROR]\033[0m %s\n' "$*"; }

usage() {
    cat <<EOF
调度服务端管理脚本

用法: $(basename "$0") {start|stop|restart|status|logs} [-h]

环境变量（可选）:
  SERVER_ADDR=$SERVER_ADDR
  DB_PATH=$DB_PATH
  SYNC_EVERY=$SYNC_EVERY
  LOG_LEVEL=$LOG_LEVEL
  TOKEN=${TOKEN:-<empty=no-auth>}
  BIN=$BIN
EOF
}

ensure_dirs() {
    mkdir -p "$RUN_DIR" "$LOG_DIR" "$(dirname "$BIN")"
    # SQLite WAL mode creates -shm/-wal siblings; the parent dir of the db
    # file must exist or the open fails with SQLITE_CANTOPEN (which modernc
    # confusingly reports as "out of memory (14)").
    mkdir -p "$(dirname "$DB_PATH")"
}

# 读取 PID 文件并校验进程存活；返回 0=存活，1=不存活
is_running() {
    [ -f "$PID_FILE" ] || return 1
    local pid
    pid="$(cat "$PID_FILE" 2>/dev/null)"
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null
}

# 二进制不存在或源码更新时自动编译，避免修改 main.go 后仍运行旧二进制
ensure_bin() {
    local need_build=0
    if [ ! -x "$BIN" ]; then
        need_build=1
    else
        # 若任一 .go 源码比二进制新，则重新编译
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

# HTTP 健康探测：能连上监听端口即视为就绪（本服务无独立 /health 接口）
wait_healthy() {
    local host port i
    host="${SERVER_ADDR%%:*}"
    port="${SERVER_ADDR##*:}"
    c_info "等待服务就绪 ($SERVER_ADDR) ..."
    for ((i = 1; i <= HEALTH_RETRIES; i++)); do
        # 优先 curl，退化到 bash 内建 /dev/tcp
        if command -v curl >/dev/null 2>&1; then
            if curl -fsS -o /dev/null --max-time 2 "http://$SERVER_ADDR/api/tasks" 2>/dev/null; then
                c_info "服务已就绪"
                return 0
            fi
        elif (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; then
            exec 3>&- 3<&- 2>/dev/null || true
            c_info "服务已就绪（端口可连）"
            return 0
        fi
        sleep "$HEALTH_INTERVAL"
    done
    c_warn "健康检查超时，请查看日志: $LOG_FILE"
    return 1
}

# ---- 生命周期命令 -----------------------------------------------------------
do_start() {
    ensure_dirs
    if is_running; then
        c_warn "服务已在运行 (PID=$(cat "$PID_FILE"))"
        return 0
    fi
    ensure_bin || return 1

    # 清理可能残留的过期 PID 文件
    rm -f "$PID_FILE"

    c_info "启动服务端: addr=$SERVER_ADDR db=$DB_PATH auth=$([ -n "$TOKEN" ] && echo yes || echo no) tls=$([ -n "$TLS_CERT" ] && echo yes || echo no) audit=$([ -n "$AUDIT_LOG" ] && echo yes || echo no) rate_limit=$RATE_LIMIT"
    nohup "$BIN" server \
        --addr "$SERVER_ADDR" \
        --db "$DB_PATH" \
        --sync-every "$SYNC_EVERY" \
        --log-level "$LOG_LEVEL" \
        ${TOKEN:+--token "$TOKEN"} \
        ${TLS_CERT:+--tls-cert "$TLS_CERT"} \
        ${TLS_KEY:+--tls-key "$TLS_KEY"} \
        ${AUDIT_LOG:+--audit-log "$AUDIT_LOG"} \
        ${RATE_LIMIT:+--rate-limit "$RATE_LIMIT"} \
        >> "$LOG_FILE" 2>&1 &
    local pid=$!
    echo "$pid" > "$PID_FILE"
    c_info "已启动 PID=$pid，日志: $LOG_FILE"

    wait_healthy || true
}

do_stop() {
    if ! is_running; then
        c_warn "服务未运行"
        rm -f "$PID_FILE"
        return 0
    fi
    local pid
    pid="$(cat "$PID_FILE")"
    c_info "发送 SIGTERM 优雅停止 PID=$pid ..."
    kill -TERM "$pid" 2>/dev/null || true

    # 等待进程退出（Go 程序会 drain 在途请求后自行退出）
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
    c_info "服务已停止"
}

do_status() {
    if is_running; then
        local pid
        pid="$(cat "$PID_FILE")"
        c_info "运行中 PID=$pid"
        if command -v curl >/dev/null 2>&1; then
            local code
            code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://$SERVER_ADDR/api/tasks" || echo 000)"
            if [ "$code" = "200" ]; then
                c_info "HTTP 健康检查正常 ($SERVER_ADDR -> 200)"
            else
                c_warn "HTTP 健康检查异常 (HTTP $code)"
            fi
        fi
        return 0
    fi
    c_warn "未运行"
    return 1
}

do_logs() {
    [ -f "$LOG_FILE" ] || { c_warn "日志文件不存在: $LOG_FILE"; return 1; }
    exec tail -n 100 -f "$LOG_FILE"
}

# ---- 入口 -------------------------------------------------------------------
case "${1:-}" in
    start)   do_start ;;
    stop)    do_stop ;;
    restart) do_stop; do_start ;;
    status)  do_status ;;
    logs)    do_logs ;;
    -h|--help|help) usage ;;
    "")
        usage
        exit 2
        ;;
    *)
        c_error "未知命令: $1"
        usage
        exit 2
        ;;
esac
