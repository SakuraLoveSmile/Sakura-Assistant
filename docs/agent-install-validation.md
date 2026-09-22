# Agent 一条命令接入验证记录

日期：2026-09-22。状态：**实现中**。

## 起点与边界

- 当前工作区 `main`，起点 `d7ea1b5`。原有 `.gitignore`、部署文件重命名及本地数据挂载、
  `docs/dependencies.md`、`hub/Dockerfile` 修改均保留；不提交或推送，不修改 Feedback。
- T1 发布、T2 中枢、T3 安装器分别由独占文件的 subagent 并行实现；主 agent 维护契约、部署、
  文档并集成验证。安装与发布均使用隔离夹具，不使用现有来源密钥或真实数据库。
- 源码实现不代表 GitHub Release、GHCR 或生产中枢已更新；外部发布和真实服务器部署尚未执行。

## 当前证据

| 层次 | 本次结果 |
|---|---|
| T1 发布流水线与不可覆盖资产 | Python 13 项测试通过；Actionlint、YAML、权限和不可变 commit 引用检查通过。GitHub CLI adapter、真实 lookup 分支、资产冲突、发布失败重试均有离线回归 |
| T2 中枢、下载校验、缓存与状态隔离 | `go test ./...`、全量 race、新增 Agent 定向 race、vet 通过；覆盖坏清单/摘要/大小、缓存修复和离线读取、并发合并/容量/活动引用、取消、来源认证隔离、命令真实 shell 执行与清理 |
| T3 安装、升级、迁移、回滚 | Python 8 组生命周期回归与 Bash 入口通过；Bash 语法、完整 ShellCheck 通过。含首次失败写队列后同来源重试、异源拒绝、旧密钥撤销后的轮换、未知配置/队列路径/服务拒绝、回滚和网络 pending |
| Linux amd64 / arm64 构建与运行 | Hub、Agent 双架构静态构建通过；Agent 两包约 10 MiB，实际文件大小及 SHA256 与发布清单一致。真实 ARM64 `--version`/`--check` 通过；本机没有原生 amd64 系统运行证据 |
| 隔离真实 systemd | Ubuntu 24.04.5 ARM64 / QEMU HVF：fresh、repeat、同来源轮换、撤销密钥拒绝、精确旧服务迁移、崩溃升级回滚、网络 pending 均通过，详细边界见下方 |
| 真实 Hub → 原始安装命令 → 真实 Agent | 独立 SQLite + HTTPS 反代 + 本地构建缓存，安装退出 0；真实 status/overview 返回在线、版本及指标，主 agent 独立复核指标序号已增长至 5 |
| GitHub Actions 正式运行 / 公开资产 | 已触发（v1.2.0 tag）：首轮因 runner 自带 shellcheck 误报 SC2317 失败，已将 CI 固定到 0.11.0 后重跑 |
| GHCR 发布 | release-hub.yml 已随 v1.2.0 tag 运行（镜像 v1.2.0 + latest + sha）；生产中枢升级仍待部署授权 |
| 真实目标服务器 / 手机效果 | 未开始，待指定并授权目标设备 |

## 完成条件

通过本地门禁和真实 systemd 验证后仍需依次验证正式资产、生产中枢下载、目标服务器指标与
手机显示；必要验证未完成时整体保持“实现中”。用户确认后才标“已验收”。

## 隔离 systemd 证据与限制

测试环境为独立 Ubuntu 24.04.5 ARM64 cloud VM，systemd 状态 `running`，不启动用户的 Docker
容器、不连接生产服务器。为此在开发机安装了 QEMU 和 ShellCheck；临时测试环境与数据不进仓库。

- 初轮使用本地可信测试 CA 的 HTTPS fixture 中枢和**真实 Agent**；未关闭 TLS 校验。
- 首装退出码 0；systemd enabled + active；目录 `0700`、环境文件 `0600`、unit `0644`；
  真实 metrics/events 被接收，SQLite 队列建立。重复安装退出码 0，序号连续增长。
- 真正密钥轮换使用已持久化的同一来源身份，新密钥安装成功；无效密钥被拒绝时不切换既有安装。
- 精确旧模板（内联 HUB/KEY，无 Wants/Type）迁移成功，密钥改由 EnvironmentFile 读取；
  来源记录 `source.json` 为 root:root / `0600`，metrics_seq / events_seq 保留。
- 故障版本为测试专用 1.2.1 程序：`--version` 可执行，但正常运行退出失败。安装器退出 1，
  恢复真实 1.2.0、原配置、enabled/active 状态，队列保留。
- 网络 pending 用临时脚本副本将等待从 120 秒缩短至 8 秒、稳定窗口改为 1 秒；启动后停止
  HTTPS fixture，退出 3，服务继续 active，实际 SQLite `metrics_q` 有待补传记录。
  此项证明失败行为，不声称默认 120 秒墙钟计时已完整等待实测。
- 首次失败后保留无密钥归属记录、同源重试/异源拒绝，由离线生命周期回归覆盖。

历史公开资产的只读下载确认 GitHub 使用 `release-assets.githubusercontent.com` 与签名查询参数，
HTTP 200、HTTPS 正常。新下载器允许该精确可信域名及其签名参数，同时拒绝 HTTP、陌生域名与
过多跳转。此证据不代表 1.2.0 新资产已经发布。

### 真实 Hub 闭环（区别于前述 fixture）

在同一隔离 VM 中保留旧夹具数据后，创建全新的 `/tmp/hub-e2e` 数据库和来源。
真实 ARM64 Hub 1.2.0 监听回环端口，经可信本地 CA 的 HTTPS 反代提供服务。
下载缓存预热自本次真实 Agent 构建，不冒充已公开的 GitHub 1.2.0 资产。

1. 真实 Hub 登录并创建 device 来源，执行响应中的 **原始 `install.command`**，交互输入测试密钥。
2. 安装退出 0；真实 `/api/v1/agent/status` 返回 Agent 1.2.0、指标序号 1 后增长到 2；
   `/api/v1/overview` 返回在线状态、主机名、CPU/内存/磁盘摘要，能力缺失正确显示 unsupported。
3. 主 agent 独立通过 SSH 查询公开健康接口、下载脚本和只读 SQLite，再次确认 Hub 1.2.0、
   指标序号 5、实际 CPU/内存/磁盘摘要，以及 systemd active/enabled。
4. 中枢实际提供的脚本 SHA256 与仓库一致：
   `628905219362b01354962020a3624dd63c188b102656b6412139ac3fbd7ef6aa`。
5. 环境文件和归属记录 `0600 root:root`，目录 `0700`，队列 DB `0600`；安装命令、unit 和
   journal 均无长期密钥。测试密钥仅用于独立临时来源，未输出或提交。

该闭环证明本地集成可用，不代替 GitHub Actions 真正发布、生产 x86_64 服务器安装或手机体验。

验证后已优雅关闭本任务 Hub、HTTPS 反代、fixture 和 QEMU VM，测试端口
2230/8795/9443/8443 均关闭。镜像、临时数据库、证书和日志保留在
`/tmp/assistant-systemd-imqInj`，双架构构建资产及临时 Actionlint 工具保留在
`/tmp/assistant-onboarding.MWPkYo`，未删除用户数据，也未启动 Docker。

## 可复跑的本地检查

在项目根目录执行脚本检查；Go 命令在相应 module 内执行：

```bash
python3 -B -m unittest discover -s scripts/agent-release -p 'test_*.py' -v
bash hub/installer/test_install.sh
bash -n hub/installer/install.sh
shellcheck hub/installer/install.sh hub/installer/test_install.sh
actionlint .github/workflows/release-agent.yml
git diff --check
```

`hub`：`go test ./...`、`go test -race ./...`、`go vet ./...`。
`agent`：`go test ./...`、`go vet ./...`，以及 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64/arm64`
分别构建 `./cmd/assistant-agent`，注入 `-X main.version=1.2.0`。
本次 Actionlint 安装在临时工具目录，不要求把新依赖写入 Go module。
