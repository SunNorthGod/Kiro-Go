# NorthGod Kiro-Go 部署与迁移

本文覆盖两种部署方式(Docker Compose / 直接二进制)以及从旧 Rust 版本迁移数据的步骤。

计费单位是**积分(credits)**:余额 = 已充值(creditsGranted) − 已消费(creditsUsed);token 仅用于统计展示。鉴权**强制常开**,必须配置至少一个卡密(API Key)才能对外服务。

---

## 一、Docker Compose 部署(推荐,自带 PostgreSQL)

前置:已安装 Docker + Docker Compose。

1. 编辑 `docker-compose.yml`,**务必改掉默认密码**:
   - `POSTGRES_PASSWORD`(postgres 服务)
   - `kiro-go` 服务里 `DATABASE_URL` 中的密码要与上面一致(host 用服务名 `postgres`)
   - `ADMIN_PASSWORD`(管理台登录密码)
2. 启动:
   ```bash
   docker compose up -d --build
   ```
3. 访问 `http://<服务器IP>:8080/admin`,用 `ADMIN_PASSWORD` 登录。

持久化:
- **账号 / 卡密 / 用量计费** → PostgreSQL(命名卷 `pgdata`)。用量走单调累加的 `usage_counters`,永不回退。
- **服务器设置 / 每日统计** → 挂载的 `./data/`(`config.json`、`daily_stats.json`)。
- 首次启动若检测到 `./data/config.json` 里已有账号/卡密且 PG 为空,会**自动一次性迁移进 PG**(见 `config.EnableDatabaseFromEnv`)。

环境变量:
| 变量 | 说明 |
|---|---|
| `DATABASE_URL` | 设置即启用 PG 持久化;不设则退回本地 JSON。`postgres://user:pass@host:5432/db?sslmode=disable` |
| `ADMIN_PASSWORD` | 管理台密码(覆盖 config.json 中的值) |
| `CONFIG_PATH` | JSON 配置路径,默认容器内 `/app/data/config.json` |
| `PORT` / `HOST` | 监听端口/地址(默认 8080 / 0.0.0.0) |

> 安全:`/v1/*` 与 `/admin` 直接暴露公网时,建议前置反向代理(Nginx/Caddy)启用 HTTPS,并限制 `/admin`、`/user` 的访问来源。

---

## 二、直接二进制部署(无 Docker)

1. 构建(需 Go >= go.mod 声明版本):
   ```bash
   GOPROXY=https://goproxy.cn,direct CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o kiro-go .
   ```
   `web/`(含 `web/vendor/`)已通过 go:embed 打进二进制,无需单独部署静态资源。
2. 运行(systemd 示例):
   ```ini
   [Service]
   Environment=ADMIN_PASSWORD=改我
   Environment=DATABASE_URL=postgres://kiro:改我@127.0.0.1:5432/kiro?sslmode=disable
   Environment=CONFIG_PATH=/opt/kiro-go/data/config.json
   ExecStart=/opt/kiro-go/kiro-go
   Restart=always
   ```
   不设 `DATABASE_URL` 时全部落 `CONFIG_PATH` 所在目录的 JSON 文件(适合单机小规模)。

---

## 三、从旧 Rust 版本迁移数据

旧 Rust 代理把数据存成 JSON 文件(通常:`credentials.json`、`api_keys.json`、`api_key_usage.json`、`api_key_recharge.json`)。用内置迁移工具 `cmd/migrate` 转换成本系统的 `config.json`,再由服务器首启时自动灌入 PG。

### 步骤
1. 把旧文件放到一个目录,例如 `./rust-data/`。
2. **先 dry-run**(默认就是 dry-run,只报告不写文件),核对读到的条数与告警:
   ```bash
   go run ./cmd/migrate -rust-dir ./rust-data
   ```
3. 确认无误后写出 `config.json`:
   ```bash
   go run ./cmd/migrate -rust-dir ./rust-data -out ./data/config.json -dry-run=false -password 你的管理密码
   ```
4. 用该 `config.json` 启动服务器:
   - **想进 PG**:设置 `DATABASE_URL` 后启动,首启会把 config.json 里的账号/卡密一次性灌入 Postgres。
   - **想留 JSON**:不设 `DATABASE_URL`,直接用 config.json 运行。

> 迁移工具**不直接写 Postgres**——它生成 config.json,复用服务器已验证过的 JSON→PG 迁移路径,避免重复实现出错。

### 字段映射与假设(重要)
- **ID**:Rust 数字 id(卡密 u32 / 账号 u64)重映射为 Go 的 UUID 字符串;`boundCredentialIds`、`parentKeyId` 会通过同一张映射表改写,引用关系保持一致。
- **优先级**:Rust `priority` 是「数字越小优先级越高」;Go 调度器的 `Weight` 是「数字越大优先级越高」。工具会**自动反转**(最小的 Rust priority → 最大的 Go Weight),保持相对顺序。全为 0 时统一给 Weight=1。
- **统一账本**:`CreditsGranted = Σ recharge.addCredits`;`CreditsUsed = Σ usage.creditsUsed`(缺失则回退 `estimatedCost`);余额 = granted − used,与 Rust 对齐。旧的固定 `creditLimit` 也会一并带过来。
- **时间戳**:同时接受 RFC3339 字符串与 unix 秒。
- **master 流量**(`apiKeyId = 0`)不产生卡密条目,其用量不计入任一卡密。
- 无对应关系的字段(如 Rust 的 `committedCredits`、Go 的 `MaxConcurrency`)会在报告中以告警列出,不会静默丢弃。

> ⚠️ 该工具是对照 Rust 源码模型编写的,**尚未在真实生产数据上跑过**。首次迁移务必先 dry-run,并在测试库上验证余额/引用无误后再上生产。

### 迁移后验证
1. 管理台能用 `ADMIN_PASSWORD` 登录。
2. 账号页:数量、优先级(Weight)、启用状态正确。
3. 卡密页:数量、余额(granted−used)、父/子卡关系、绑定账号正确。
4. 概览页统计与预期量级一致。
5. 用真实客户端(插件 / opencode 等)打一次 `/v1/messages` 与 `/v1/chat/completions` 冒烟。
