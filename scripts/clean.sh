#!/usr/bin/env bash
#
# clean.sh — 一键清除运行时产物（保留启动脚本和源代码）
#
# 清理范围:
#   bin/                  编译产物（二进制）
#   run/                  PID 文件
#   logs/                 日志文件
#   *.db *.db-shm *.db-wal SQLite 数据库及 WAL/SHM 临时文件
#   data/                 持久化数据库目录（如果存在）
#
# 不清理:
#   scripts/              启动脚本（start_server.sh / start_worker.sh）
#   internal/              源代码
#   main.go go.mod go.sum 源代码与依赖
#   .git/ .gitignore       版本控制
#
# 用法:
#   ./scripts/clean.sh            清理运行时产物
#   ./scripts/clean.sh -y         跳过确认提示
#   ./scripts/clean.sh --stop     先停止运行中的服务再清理
#   ./scripts/clean.sh -h         帮助
#
set -uo pipefail

# ---- 路径 ---------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT_DIR"

# ---- 工具 ---------------------------------------------------------------
c_info()  { printf '\033[32m[INFO]\033[0m  %s\n' "$*"; }
c_warn()  { printf '\033[33m[WARN]\033[0m  %s\n' "$*"; }
c_error() { printf '\033[31m[ERROR]\033[0m %s\n' "$*"; }

# ---- 参数解析 -----------------------------------------------------------
ASSUME_YES=0
STOP_FIRST=0
while [ $# -gt 0 ]; do
    case "$1" in
        -y|--yes) ASSUME_YES=1; shift ;;
        --stop)   STOP_FIRST=1; shift ;;
        -h|--help)
            sed -n '3,20p' "$0" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *) c_error "未知参数: $1"; exit 2 ;;
    esac
done

# ---- 可选：先停止运行中的服务 ------------------------------------------
if [ "$STOP_FIRST" = "1" ]; then
    if [ -x "$SCRIPT_DIR/start_server.sh" ]; then
        c_info "停止 server ..."
        "$SCRIPT_DIR/start_server.sh" stop 2>&1 | sed 's/^/  /' || true
    fi
    if [ -x "$SCRIPT_DIR/start_worker.sh" ]; then
        c_info "停止 worker ..."
        "$SCRIPT_DIR/start_worker.sh" stop 2>&1 | sed 's/^/  /' || true
        # 多实例兜底：扫描所有 worker-*.pid
        for pf in "$ROOT_DIR"/run/worker-*.pid; do
            [ -f "$pf" ] || continue
            id="$(basename "$pf" .pid | sed 's/^worker-//')"
            [ "$id" = "worker-1" ] && continue  # 上面已停
            c_info "停止 worker[$id] ..."
            "$SCRIPT_DIR/start_worker.sh" stop -i "$id" 2>&1 | sed 's/^/  /' || true
        done
    fi
fi

# ---- 待清理项清单 -------------------------------------------------------
# 用函数收集，便于打印和实际清理
declare -a TARGETS=()
add_target() {
    local pat="$1"
    local desc="$2"
    shopt -s nullglob
    local matches=( $pat )
    shopt -u nullglob
    if [ ${#matches[@]} -gt 0 ]; then
        TARGETS+=("${matches[@]}")
        c_info "将清理 $desc: ${#matches[@]} 项"
    fi
}

c_info "扫描清理目标 ..."
add_target "bin"                     "编译产物 bin/"
add_target "run"                      "PID 文件 run/"
add_target "logs"                     "日志 logs/"
add_target "data"                     "持久化数据库目录 data/"
add_target "*.db"                     "SQLite 数据库 *.db"
add_target "*.db-shm"                 "SQLite 共享内存 *.db-shm"
add_target "*.db-wal"                 "SQLite WAL *.db-wal"

if [ ${#TARGETS[@]} -eq 0 ]; then
    c_info "没有需要清理的运行时文件"
    exit 0
fi

# ---- 确认 ---------------------------------------------------------------
if [ "$ASSUME_YES" != "1" ]; then
    printf '\n\033[33m将删除以上 %d 项（启动脚本和源代码不受影响）。继续? [y/N]\033[0m ' "${#TARGETS[@]}"
    ans=""
    read -r ans
    case "${ans,,}" in
        y|yes) ;;
        *) c_info "已取消"; exit 0 ;;
    esac
fi

# ---- 执行清理 -----------------------------------------------------------
c_info "开始清理 ..."
removed=0
for t in "${TARGETS[@]}"; do
    if [ -e "$t" ] || [ -L "$t" ]; then
        rm -rf -- "$t"
        removed=$((removed + 1))
    fi
done
c_info "完成：已清理 $removed 项"

# ---- 残留进程检查（安全提示） -------------------------------------------
shopt -s nullglob
leftover_pids=$(pgrep -f "bin/worker_lightweight (server|worker)" 2>/dev/null || true)
shopt -u nullglob
if [ -n "$leftover_pids" ]; then
    c_warn "仍有 worker_lightweight 进程在运行（未通过 --stop 停止）:"
    echo "$leftover_pids" | tr ' ' '\n' | sed 's/^/  PID /'
    c_warn "如需强制结束: pkill -f 'bin/worker_lightweight'"
fi
