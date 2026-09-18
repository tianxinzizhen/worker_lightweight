# worker_lightweight

一个轻量级分布式任务调度系统，使用 Go 编写。由 **Server**（HTTP API + cron 调度器 + SQLite 持久化 + Web UI）和 **Worker**（拉取式执行池）两部分组成，支持一次性任务和定时任务，具备超时、重试、暂停/恢复、停止/重启、多字段过滤与分页等能力。

## 技术栈

| 组件 | 说明 |
| --- | --- |
| 语言 | Go 1.24 |
| 存储 | SQLite（`modernc.org/sqlite`，纯 Go 驱动，WAL 模式） |
| 定时 | `robfig/cron/v3`（6 字段 cron 表达式，含秒） |
| Web | 标准库 `net/http` + `html/template` |

## 架构

```
┌──────────────────────────────────────────────────────┐
│                     Server                            │
│  ┌──────────┐  ┌───────────┐  ┌───────────────────┐  │
│  │ HTTP API │  │ Cron 调度 │  │   Web UI (/)      │  │
│  └────┬─────┘  └─────┬─────┘  └─────────┬─────────┘  │
│       │              │                   │            │
│       └──────────────┼───────────────────┘            │
│                      ▼                                │
│            ┌──────────────────┐                       │
│            │  Store (SQLite)  │                       │
│            └────────┬─────────┘                       │
└─────────────────────┼─────────────────────────────────┘
                      │  HTTP (/api/pull, /api/result, /api/worker/heartbeat)
          ┌───────────┴───────────┐
          ▼                       ▼
    ┌──────────┐            ┌──────────┐
    │ Worker 1 │  ...       │ Worker N │
    │ 执行池   │            │ 执行池   │
    └──────────┘            └──────────┘
```

- **Server** 是任务的唯一真相来源。Worker 通过 `POST /api/pull` 原子地认领下一条到期的 `pending` 任务（SQL `UPDATE ... WHERE status='pending'` 保证不重复认领），执行完毕后通过 `POST /api/result` 上报结果。
- **Cron 任务**（`type=cron`）本身不会被执行；调度器按 cron 表达式周期性地为其生成一次性子任务（`parent_id` 指向父任务），由 Worker 认领执行。
- **暂停 cron**（`POST /api/tasks/{id}/pause`）会在同一事务内将所有进行中的子任务标记为 `stopped`，Worker 在下一次 pull 时通过 `stop_list` 取消这些在途任务。

## 任务状态

```
pending ──claim──▶ running ──success──▶ success
                     │
                     ├──fail, 重试次数≤max_retry──▶ pending（指数退避）
                     ├──fail, 重试次数>max_retry──▶ dead
                     └──用户 stop ──▶ stopped ──restart──▶ pending
```

状态值：`pending` / `running` / `success` / `failed` / `dead` / `stopped`。

## 快速开始

### 1. 编译

```bash
go build -o bin/worker_lightweight .
```

### 2. 启动 Server

```bash
./bin/worker_lightweight server \
  --addr 127.0.0.1:8080 \
  --db   ./data/tasks.db \
  --sync-every 30s \
  --log-level info
```

启动后访问 <http://127.0.0.1:8080/> 打开 Web 控制台。

### 3. 启动 Worker

```bash
./bin/worker_lightweight worker \
  --server http://127.0.0.1:8080 \
  --id     worker-1 \
  --workers 4 \
  --pull-every 1s \
  --log-level info
```

可启动多个 Worker（不同 `--id`）组成执行集群。

### 4. 创建任务

```bash
# 一次性任务（立即执行）
curl -X POST http://127.0.0.1:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{"name":"hello","command":"echo hello world","type":"once"}'

# 带超时与重试的一次性任务
curl -X POST http://127.0.0.1:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{"name":"fail","command":"exit 1","type":"once","max_retry":3,"timeout_sec":10}'

# 定时任务（每秒触发，6 字段 cron 表达式）
curl -X POST http://127.0.0.1:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{"name":"heartbeat","command":"echo beat","type":"cron","cron_expr":"* * * * * *"}'
```

## 命令行参数

### `server` 模式

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--addr` | `127.0.0.1:8080` | HTTP 监听地址 |
| `--db` | `./tasks.db` | SQLite 数据库文件路径 |
| `--sync-every` | `30s` | cron 调度表从数据库重同步的间隔 |
| `--log-level` | `info` | 日志级别：`debug` \| `info` |

### `worker` 模式

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--server` | `http://127.0.0.1:8080` | Server 地址 |
| `--id` | `worker-1` | Worker 唯一标识（上报结果与心跳时使用） |
| `--workers` | `4` | 执行协程池大小 |
| `--pull-every` | `1s` | 拉取任务的轮询间隔 |
| `--log-level` | `info` | 日志级别：`debug` \| `info` |

> 两种模式均响应 `SIGINT` / `SIGTERM` 进行优雅停止：Server 会等待在途请求完成（最长 10s）；Worker 会排空任务池后退出。

## HTTP API

### 任务管理

#### 创建任务

`POST /api/tasks`

请求体（`CreateTaskRequest`）：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `name` | string | 是 | 任务名称 |
| `command` | string | 是 | 执行的 shell 命令（通过 `/bin/sh -c` 运行） |
| `type` | string | 是 | `once` 或 `cron` |
| `cron_expr` | string | cron 必填 | 6 字段 cron 表达式（含秒），如 `* * * * * *` |
| `max_retry` | int | 否 | 失败最大重试次数，默认 0 |
| `timeout_sec` | int | 否 | 执行超时秒数，0 表示 5 分钟默认超时 |

返回：`201` + 创建的任务 JSON。

#### 任务列表（分页 + 过滤）

`GET /api/tasks`

查询参数：

| 参数 | 说明 |
| --- | --- |
| `page` | 页码，默认 1 |
| `page_size` | 每页条数，默认 20，上限 500 |
| `name` | 按名称模糊匹配 |
| `command` | 按命令模糊匹配 |
| `type` | 按类型精确匹配（`once` \| `cron`） |
| `status` | 按状态过滤，逗号分隔（如 `pending,running`），或使用别名 `done`（匹配 success/failed/dead） |
| `worker` | 按 worker_id 模糊匹配 |
| `enabled` | `true` \| `false` |
| `parent_id` | 0 仅显示顶层任务（默认）；>0 显示某 cron 的子任务 |

返回：

```json
{
  "tasks": [ ... ],
  "total": 100,
  "page": 1,
  "page_size": 20,
  "total_pages": 5
}
```

#### 单个任务操作

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/tasks/{id}` | 获取任务详情 |
| `DELETE` | `/api/tasks/{id}` | 删除任务 |
| `POST` | `/api/tasks/{id}/pause` | 暂停 cron 任务（同时停止在途子任务） |
| `POST` | `/api/tasks/{id}/resume` | 恢复已暂停的 cron 任务 |
| `POST` | `/api/tasks/{id}/stop` | 停止运行中/待执行的任务实例 |
| `POST` | `/api/tasks/{id}/restart` | 将 stopped 状态的任务恢复为 pending |

### Worker 接口

#### 认领任务

`POST /api/pull?worker_id={id}`

返回 `PullResponse`：

```json
{
  "task": { ... },        // 无任务时为 null
  "stop_list": [12, 34]   // 该 worker 拥有、已被标记 stopped 的任务 id，需取消执行
}
```

#### 上报执行结果

`POST /api/result`

请求体（`Result`）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `task_id` | int64 | 任务 id |
| `worker_id` | string | 执行该任务的 worker id（必须与认领者一致） |
| `success` | bool | 是否成功 |
| `output` | string | 成功时为 stdout，失败时为 stderr/错误信息（截断至 4096 字符） |

> 若任务执行期间被标记为 `stopped`，上报结果时不会覆盖该状态，仅解绑 worker。

#### Worker 心跳

`POST /api/worker/heartbeat`

请求体（`WorkerInfo`）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | worker id |
| `total_slots` | int | 执行池总槽位数 |
| `active_count` | int | 当前活跃任务数 |
| `running_tasks` | []int64 | 正在执行的任务 id 列表 |
| `started_at` | datetime | 启动时间 |

Worker 默认每 3 秒上报一次；超过 60 秒未心跳的 worker 会从在线列表中移除。

#### 在线 Worker 列表

`GET /api/workers`

返回最近一次心跳仍有效的 worker 列表。

## Web UI

| 路径 | 说明 |
| --- | --- |
| `/` | 任务控制台：任务列表（分页、过滤）、在线 Worker 状态面板、新建任务弹窗（含前端校验） |
| `/tasks/{id}` | 任务详情页（含 cron 子任务执行历史） |

控制台特性：

- 任务列表支持按名称、命令、类型、状态、worker、启用状态过滤，结果分页展示
- Worker 面板实时显示各节点的槽位占用与运行中的任务
- 新建任务弹窗对必填项、cron 表达式（6 字段）、`max_retry`（0–100）、`timeout_sec`（≥0）做前端校验，服务端也会校验 cron 语法
- 一次性任务支持 Stop / Restart；cron 任务支持 Pause / Resume
- 操作结果通过右下角 toast 提示，2.5 秒后自动消失

## 项目结构

```
.
├── main.go                     # 入口：server / worker 两种模式
├── go.mod
├── scripts/
│   ├── start_server.sh         # Server 启动/停止/重启/状态/日志
│   ├── start_worker.sh         # Worker 启动/停止/重启/状态/日志（支持 -i 指定多实例）
│   └── clean.sh                # 清理运行时产物（bin/run/logs/data/*.db）
└── internal/
    ├── model/task.go           # Task / Result / PullResponse / WorkerInfo 等数据结构
    ├── store/store.go          # SQLite 持久化（CRUD、认领、结果上报、停止列表）
    ├── server/
    │   ├── server.go           # HTTP 路由与请求处理
    │   ├── scheduler.go        # cron 调度：为 cron 定义生成一次性子任务
    │   ├── ui.go               # Web 控制台与详情页渲染
    │   └── ui_template.go      # UI HTML/JS 模板
    ├── worker/pool.go          # 拉取式执行池（fetcher + 固定数量 executor + 心跳）
    ├── lock/memory.go          # 内存分布式锁（防止 cron 重复生成子任务）
    └── logger/logger.go        # 结构化日志
```

## 启动脚本

仓库提供了开箱即用的管理脚本，会在源码更新时自动重新编译二进制。

### Server

```bash
./scripts/start_server.sh {start|stop|restart|status|logs}
```

可用环境变量覆盖默认值：`SERVER_ADDR`、`DB_PATH`（默认 `./data/tasks.db`）、`SYNC_EVERY`、`LOG_LEVEL`、`BIN`。

### Worker

```bash
./scripts/start_worker.sh {start|stop|restart|status|logs} [-i worker-id]
```

`-i` 用于指定 worker id，支持同机多实例（PID 与日志按 id 区分）。环境变量：`SERVER_ADDR`、`WORKER_ID`、`WORKERS`、`PULL_EVERY`、`LOG_LEVEL`、`BIN`。

### 清理

```bash
./scripts/clean.sh            # 清理 bin/ run/ logs/ data/ *.db（需确认）
./scripts/clean.sh -y         # 跳过确认
./scripts/clean.sh --stop     # 先停止运行中的服务再清理
```

## 设计要点

- **原子认领**：`ClaimPending` 在事务内用 `UPDATE ... WHERE status='pending'` 完成状态翻转，`RowsAffected=0` 表示竞争失败，保证同一任务最多被一个 worker 认领。
- **WAL + 单连接池**：SQLite 开启 WAL 模式提升并发写能力，连接池设为 1 以避免多连接写锁竞争。
- **cron 子任务隔离**：cron 定义行本身不进入执行流，每次触发新建一个 `type=once`、`parent_id=父id` 的子任务，重试与结果互不影响。
- **停止传播**：暂停 cron 或停止任务时，行状态置为 `stopped`；worker 下一次 pull 会拿到包含该 id 的 `stop_list`，通过 `context.CancelFunc` 中断在途进程。
- **重试退避**：失败且未超 `max_retry` 时，任务回到 `pending`，`next_run_at` 设为 `now + retry_count*5s`，避免失败风暴。
