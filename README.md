# Linux.do 指定主题 / 指定用户回复邮件监控（Go）

> **Go + Discourse JSON + SQLite + SMTP + Docker**。支持多个监控规则、Web 后台动态维护、无游标自动初始化、持久化邮件队列、失败重试、超长主题按楼层窗口读取。

## 快速开始

```bash
cp .env.example .env
# 用 nano / vim 修改 .env，至少配置 ADMIN_PASSWORD 和 SMTP_* 邮件参数
docker compose up -d --build
docker compose logs -f --tail=100
```

默认 **只绑定 VPS 的 `127.0.0.1:8080`**，不直接向公网暴露管理密码。使用 SSH 隧道访问：

```bash
ssh -L 8080:127.0.0.1:8080 user@你的VPS地址
```

浏览器打开 `http://127.0.0.1:8080`，输入 `.env` 中的 `ADMIN_USER` / `ADMIN_PASSWORD`。若想通过公网访问，请配置 HTTPS + 反向代理（Nginx、Caddy 均可）。

## 动态配置监控规则

Web 页面支持：

- 新增：主题 URL + 用户名，支持同时维护多条不同组合；
- 修改：改主题或用户名会清空该规则原有邮件记录并重新建立游标；
- 启用 / 暂停 / 删除、立即检查；
- 展示已处理楼层、最近检查、异常和待发/已发邮件；
- SMTP 测试、主题 API 连通性检查。

示例输入：

- 主题链接：`https://linux.do/t/topic/2696141/33528`
- 用户名：`Andrew-hh`
- 收件邮箱：`.env` 的 `SMTP_TO`，无需放入规则；可填写多个邮箱（逗号分隔）。

`.env` 中的 `SEED_TOPIC_URL` 和 `SEED_USERNAME` 可在**首次启动且数据库空白时**自动创建上述监控规则；在页面删除后重启不会再次添加。设置环境变量并非必要，可以全部从页面新增。

## 没有设置帖子游标时怎么办？

**完全不用设置。** 新规则 `initialized=0`，第一次成功抓取当前主题信息时，保存接口返回的末尾帖子 ID（如有）与 `highest_post_number`，并把 `initialized` 设为 1。**不会推送历史内容。** 下一轮仅检查新回复。网络失败、403/429 时保持 `initialized=0`，自动退避重试。SQLite 重启持久保留游标。

程序用 **`cursor_id` + `cursor_number` 两个游标**：常规主题优先用帖子 ID 流抓取；超过 1 万楼的 Discourse 超长主题可能不返回完整 ID 流，此时按 `post_number` 通过 `?post_number=N` 窗口继续抓取，直到追上最新楼层。无有效下一窗口时**停止推进游标并报警**，而不是静默跳过。每次轮询最多处理 `MAX_NEW_PER_POLL` 条，剩余部分下轮接续。

> 注意：首次基线建立发生在**首次成功访问接口时**，不是规则提交那一瞬间。首次成功前出现的回复视为历史回复；如需精确从创建时起覆盖，需让 API 立即可用。删除/不可见楼层和社区版本差异可能造成窗口空缺，必须查看错误状态排查。

## 邮件保真与去重

- 发信内容使用 Discourse 的 `cooked` HTML，经 HTML 安全清洗后保留文字、常见排版、图片和链接（相对链接补全 `https://linux.do`），同时生成纯文本备用部分。外部图片需要邮箱客户端允许加载；视频 iframe、脚本和部分复杂论坛组件不会嵌入邮件，可点直达链接查看原帖。
- 包含主题、作者、发表时间、楼层及 **直达** `https://linux.do/t/topic/<topic_id>/<post_number>` 的链接。
- `deliveries` 表对 `(rule_id, post_id)` 加唯一约束；一次抓取的邮件入队和游标推进在**同一个 SQLite 事务**提交。
- 发送失败依指数退避重新尝试（最长约 6 小时），邮件数据保存在 SQLite，不会因为重启丢失。
- 已成功提交 SMTP 才标记 `sent`；SMTP 的提交与本地数据库无法实现分布式原子事务，极少数“SMTP 已接收但进程立即崩溃”场景**仍有重复邮件风险**，所以这是至少一次交付保障，并非严格的端到端恰好一次。

## 邮箱配置

| 变量 | 含义 | 示例 |
| --- | --- | --- |
| `SMTP_HOST` | SMTP 服务器 | `smtp.qq.com` |
| `SMTP_PORT` | SMTP 端口 | `465` |
| `SMTP_TLS_MODE` | `implicit`(465) / `starttls`(587) | `implicit` |
| `SMTP_USER` | 邮件账户 | `12345678@qq.com` |
| `SMTP_PASSWORD` | SMTP 授权码，不一定是邮箱登录密码 | 你获得的授权码 |
| `SMTP_FROM` | 邮件发件地址 | `12345678@qq.com` |
| `SMTP_TO` | 通知收件地址 | `you@example.com` |

QQ/163 等通常要求启用 SMTP 并使用授权码；各邮箱端口以服务商文档为准。网络连接启用 TLS 证书校验，拒绝通过明文连接传输 SMTP 密码。

## 错误、限制和安全

- Linux.do 可能对服务器请求启用 Cloudflare 或 Discourse API 限流；请在 VPS 实际执行前端页面的“测试主题接口”验证。**无法保证无需授权的所有节点都能通过**。程序不尝试绕过 Cloudflare、登录或权限限制。
- 403、429、超时、格式异常均不会推进游标；轮询指数退避 1 分钟至 1 小时，并优先遵守更长的 `Retry-After`（上限 24 小时）。
- 抓取线程与 SMTP 发送线程分离，邮件服务器暂时故障不会阻塞新增回复写入队列；单实例不建议配置过多规则，以免触发社区限流。
- `POLL_INTERVAL` 默认 5 分钟，最短 1 分钟。推荐 3～10 分钟，避免高频请求。
- 后台使用 HTTP Basic Auth + CSRF 校验；**请仅在 HTTPS 代理后或 SSH 隧道中访问**。不建议直接映射到 `0.0.0.0:8080` 后明文登录。
- 项目只接受 `https://linux.do` 链接作为规则输入，避免任意 URL SSRF；Docker 默认只读根文件系统、非 root 用户运行，数据库置于持久化命名卷。
- 默认单实例运行；**不要同时启动多个容器共享同一 SQLite 文件**。

## 日常命令

```bash
# 日志
docker compose logs -f --tail=100

# 更新镜像，保留 SQLite 数据
docker compose up -d --build

# 停止容器，不删除 SQLite 数据
docker compose down

# 检查服务健康度
curl http://127.0.0.1:8080/healthz

# 用 Go 在可以访问 Go 模块代理的机器上单独运行测试
go test ./... -v
```

**备份数据库：** 最简单方法是先 `docker compose down`，再备份 Docker volume `monitor_data` 下的 `monitor.sqlite3`。请勿在运行状态下只复制主 `.sqlite3` 文件而忽略 WAL。

## 源码结构

- `main.go` 程序启动与优雅退出
- `config.go` 环境变量、输入校验
- `discourse.go` API 抓取、规则 URL/用户名解析
- `worker.go` 规则轮询、超长主题翻页、错误退避和邮件队列消费
- `db.go` SQLite schema、游标和消息入队事务
- `mailer.go` HTML+纯文本 MIME、SMTP TLS、发送失败恢复
- `web.go` 无前端框架的管理界面（Go `html/template`）
- `discourse_test.go` 规则解析、基线、超长主题空 ID 流回退、普通 ID 流、429 响应、邮件内容与重试队列单测

## 构建说明

首次 Docker 构建必须能下载 Go 模块（`modernc.org/sqlite` 为纯 Go SQLite 驱动，`CGO_ENABLED=0`）。Docker 构建采用 Go 1.27 和 Alpine 3.24；本仓库未内置 Go 依赖包或编译好的二进制；服务器若无法访问 Go 官方代理，可按你可信任的镜像源配置 `GOPROXY` 后构建。

## 验证状态与上线前检查

- 本项目在离线构建环境中完成 Go 源码语法解析、`gofmt` 格式化，并通过了**仅使用 Go 标准库的 Discourse JSON/URL 核心测试**。
- 当前环境无法下载第三方依赖，因此**尚未在本环境完成 `go test ./...`、完整二进制构建和 SMTP/真实 Linux.do 端到端测试**。首次在 VPS 执行 `docker compose up -d --build` 需要联网拉取依赖；启动后按 Web 界面依次测试主题接口、发送测试邮件，并检查初始化后的楼层是否正确。不要在测试完成前假定零漏报。
- 由于 Linux.do 可能启用 Cloudflare 访问限制，真实环境若出现 403/429 应等待或联系站点确认允许的访问方式；本服务不包含绕过防护的手段。
