# 安全加固待办清单

本文档记录 worker_lightweight 项目的安全问题与加固计划，按优先级排序。
状态标记：`待处理` / `进行中` / `已完成`

---

## P0 — 必须立即修复（未授权即可 RCE / 数据泄露）

### P0-1 API 与 Web UI 鉴权

- **问题表现**：全项目无任何认证机制，任何人访问 API 端口即可创建/删除/停止任务、读取所有任务与执行结果。
- **影响范围**：所有 `/api/*` 接口与 Web UI 页面（`/`、`/tasks/`）。
- **加固方案**：引入共享密钥（token）机制。
  - Server 启动时通过 `--token` 指定密钥；为空则不启用鉴权（兼容本地单机场景）。
  - 所有 `/api/*` 请求需携带 `Authorization: Bearer <token>`（或 `X-Token` 头），否则返回 401。
  - Web UI 页面访问需通过 `?token=` 查询参数或请求头携带密钥，校验通过后将 token 嵌入页面供 JS 调用。
- **状态**：已完成

### P0-2 Worker 身份认证

- **问题表现**：`/api/pull`、`/api/result`、`/api/worker/heartbeat` 中的 `worker_id` 仅为自报字符串，无凭证校验。攻击者可伪装 Worker 拉走任务、上报虚假结果、污染监控面板。
- **影响范围**：Worker 与 Server 之间的全部 HTTP 交互。
- **加固方案**：Worker 启动时通过 `--token` 指定与 Server 一致的密钥，所有请求携带 `Authorization: Bearer <token>` 头。Server 在校验 token 后才处理 Worker 请求。
- **状态**：已完成

---

## P1 — 高优先级（传输安全 / 网络暴露）

### P1-1 启用 TLS（HTTPS）

- **问题表现**：Server 与 Worker 之间全程明文 HTTP，Worker ID、任务命令、执行结果（可能含敏感信息）可被内网嗅探。
- **影响范围**：所有 HTTP 流量。
- **加固方案**：
  - Server 支持 `--tls-cert` / `--tls-key` 参数启用 HTTPS。
  - Worker 通过 `https://` 连接 Server，默认验证证书；自签名证书场景可加 `--insecure` 跳过验证。
  - 无证书场景下建议前置反向代理（Nginx/Caddy）终止 TLS。
- **状态**：已完成

### P1-2 网络隔离与监听地址

- **问题表现**：默认监听 `127.0.0.1`，但若配置为 `0.0.0.0` 则可能暴露到不可信网络。
- **影响范围**：API 可达性。
- **加固方案**：
  - 代码层面：`runServer()` 在监听非 loopback 地址时，自动输出安全警告日志（无 TLS / 无 token 各一条）。
  - 文档明确建议仅在内网或本机监听。
  - 生产环境通过防火墙 / 安全组限制访问源。
  - 公网场景必须前置鉴权 + TLS + 反向代理。
- **状态**：已完成

---

## P2 — 中优先级（运行时隔离 / 可审计）

### P2-1 命令执行沙箱化

- **问题表现**：Worker 以自身进程权限通过 `/bin/sh -c` 直接执行任务命令，无隔离，单个恶意任务可影响宿主机或同机其他任务。
- **影响范围**：所有由 Worker 执行的任务。
- **加固方案**：
  - 使用受限系统用户运行 Worker（最小权限）——文档提示。
  - Worker 增加 `--allowed-commands`（允许名单正则）和 `--blocked-commands`（禁止名单正则），在 `pool.exec()` 中编译后校验；不匹配/匹配的命令直接拒绝，不会触及 shell。
  - 正则在 Worker 启动时编译，编译失败直接报错退出（fail-fast）。
- **状态**：已完成

### P2-2 操作审计日志

- **问题表现**：仅有任务执行日志，缺少「谁在何时对哪个任务做了什么操作」的审计记录，出问题无法追溯。
- **影响范围**：任务创建、删除、暂停、恢复、停止、重启等操作。
- **加固方案**：
  - Server 新增 `--audit-log` 参数，指向独立审计日志文件（文件权限 0600）。
  - 启用后，`/api/tasks*`、`/api/pull`、`/api/result` 请求全部经过 `auditMiddleware`，记录：时间、来源 IP（支持 X-Forwarded-For）、HTTP 方法、路径、task_id、状态码。
  - 使用 `captureWriter` wrapper 捕获响应状态码，`clientIP()` 工具函数提取客户端地址。
- **状态**：已完成

### P2-3 数据库文件权限

- **问题表现**：SQLite 数据文件以默认权限创建，可能被同机其他用户读取，包含任务命令与结果。
- **影响范围**：`tasks.db` 及其 WAL/SHM 文件。
- **加固方案**：
  - `store.New()` 在 migrate 后调用 `secureDB()`，将 `.db` / `-wal` / `-shm` / `-journal` 文件 chmod 为 0600。
  - 数据目录 chmod 为 0700。
  - 所有 chmod 错误静默忽略（非致命）——某些只读挂载/卷场景下无法修改。
- **状态**：已完成

---

## P3 — 低优先级（可用性 / 滥用防护）

### P3-1 API 速率限制

- **问题表现**：API 无任何限流，可被用于 DoS 或暴力枚举。
- **影响范围**：所有 `/api/*` 接口。
- **加固方案**：
  - Server 新增 `--rate-limit` 参数（每分钟每 IP 请求上限，0=不限）。
  - 实现 per-IP token bucket（`rateLimiter`，`sync.Mutex` 保护），初始满桶，线性补给。
  - `rateLimitMiddleware` 作为最外层 middleware，拒绝时返回 429 + `Retry-After: 60`。
  - 使用 `clientIP()` 统一提取 IP（支持 X-Forwarded-For）。
- **状态**：已完成

---

## 已验证安全项（无需改动）

- **无 SQL 注入**：`internal/store/store.go` 全部使用参数化查询（`?` 占位符），`buildWhere` 仅拼接白名单列名。
- **XSS 已缓解**：Web UI 列表渲染中 `name`/`command`/`result`/`worker_id` 等用户可控字段均经 `escapeHtml` 转义。
- **任务认领原子性**：`ClaimPending` 通过 `UPDATE ... WHERE status='pending'` 行锁保证同一任务最多被一个 Worker 认领。
