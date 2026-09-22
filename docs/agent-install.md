# 服务器一条命令接入（中枢 ≥ 1.2.0）

每台设备单独创建一个「设备采集」来源，运行一个 Agent。设备主动通过 HTTPS 向中枢上报，
无需开放入站端口，安装和下载也只访问中枢域名。中枢需要能访问 GitHub Release 及其资产 CDN；
已校验的本地缓存可在上游临时不可达时继续使用。

## T1 发布与部署准备

源码有安装器不等于线上链接已经可用。发布前必须完成 `agent`、`hub` 的测试与构建，
以及安装器自动检查和隔离 systemd 验证。下一正式版本为 `v1.2.0`，不要覆盖已发布的 `v1.1.0`。

1. 获授权后发布正式 `vX.Y.Z` 标签。Agent workflow 编译 amd64、arm64 静态二进制，注入相同版本。
2. Release 包含 `assistant-agent_X.Y.Z_linux_amd64`、`assistant-agent_X.Y.Z_linux_arm64`、
   `agent-SHA256SUMS.txt`，最后发布 `agent-manifest.json`。已有 APK/Hub 资产保持不变；
   同名资产只允许内容一致时跳过，禁止覆盖。同一版本下载失败应排查，不通过覆盖 Release 修补。
3. 确认 Agent 资产完整，再部署该版本 Hub 镜像。原 Hub 镜像工作流仍由正式标签独立构建；
   镜像构建成功不代表 Agent 资产发布成功，不要先切换生产中枢。
4. 在中枢环境中指定以下非敏感配置（镜像版本带 `v`，Agent 版本不带 `v`）：

   ```dotenv
   HUB_VERSION=v1.2.0
   ASSIST_AGENT_VERSION=1.2.0
   ASSIST_BASE_URL=https://hub.example.com
   ASSIST_AGENT_CACHE_DIR=/data/agent-cache
   ASSIST_AGENT_CACHE_MAX_BYTES=536870912
   ```

   Compose 已将 `/data` 持久化；缓存不含来源凭证，可独立清理，不要删除同目录下的中枢数据库。
   `ASSIST_AGENT_VERSION` 固定经过验证的版本，不跟随 GitHub latest。
5. 验证公开端点返回真实文件，而非反代登录页或 HTML 错误页：

   ```bash
   curl -fsS https://hub.example.com/api/v1/agent/releases/stable
   curl -fsS https://hub.example.com/api/v1/agent/install.sh -o /tmp/assistant-install-review.sh
   bash -n /tmp/assistant-install-review.sh
   ```

反代须放行 `/api/v1/agent/`，保持二进制原样传输。设备不需要 GitHub 账号或个人访问令牌。
首次下载由中枢从固定仓库拉取；无缓存且上游不可达时明确失败，不降级到其他下载站。

## T2 首次接入

支持 Linux `x86_64`/`amd64` 与 `aarch64`/`arm64`，且 PID 1 是 systemd。
准备 Bash、curl、jq、sha256sum、timeout、flock、systemd 及常用 Linux 基础命令。
Debian/Ubuntu 如缺依赖，可由管理员安装 `curl ca-certificates jq coreutils util-linux`；
安装器不会自动执行软件包安装、修改软件源或关闭 TLS 校验。

1. App 首页 → 接入管理 → 新建来源 → 选择「设备采集」，每台机器使用独立来源，附件回连地址留空。
2. 保持一次性密钥对话框，复制其中完整安装命令到该服务器终端执行。
3. 按提示粘贴来源密钥，输入时不回显；不要把密钥改成命令参数、URL、聊天消息或工单内容。
4. 安装器下载校验后配置 systemd，最多等待 120 秒确认新的指标序号及目标 Agent 版本。
   确认成功后回到手机首页查看在线状态及真实指标。若只显示“待确认上报”，依排障说明检查，
   不把 service active 当作已接入。

不需要手动编译、上传二进制、填写登录账号密码或安装新版 APK。安装器以 root 服务采集宿主机；
Docker socket、SMART 等能力仍取决于本机工具和设备权限，不支持的能力不会伪装为正常。
SMART 使用 `smartctl -n standby`，不会主动唤醒休眠硬盘。

## T3 升级与密钥轮换

重新运行安装脚本默认复用已有配置并安装中枢选定的稳定版本，不会后台自动升级。
已下载的脚本也支持以下参数（示例中的域名请替换为实际中枢；不含任何密钥）：

```bash
sudo bash /path/to/install.sh --hub https://hub.example.com
sudo bash /path/to/install.sh --hub https://hub.example.com --version 1.2.0
sudo bash /path/to/install.sh --hub https://hub.example.com --reconfigure
```

- `--version` 只接受正式 `X.Y.Z`；不允许无意降级。新版本程序通过下载、大小、摘要和内部版本
  检查后才停止旧服务。并发安装会被锁阻止。
- 配置目录 `/etc/assistant-agent` 为 `0700`，`agent.env` 为 `0600`；systemd unit 使用
  `EnvironmentFile`，不再内联明文密钥。不要用 `cat agent.env` 或展开环境变量来收集排障日志。
- 同目录的 `source.json` 只保存中枢地址和来源 ID，不含密钥。首次安装失败时保留此归属记录，
  让已经产生的队列可以在修复版本后用同一来源安全重试；不要删除该记录来绕过来源检查。
- 队列目录 `/var/lib/assistant-agent` 含 SQLite、WAL、序号和待上报事件，升级不删改其内容。
  程序启动失败会恢复旧程序、配置与服务；网络暂时中断不会触发数据回退，服务保留队列继续补传。
- 在 App 中轮换已有设备密钥后，运行新命令（含 `--reconfigure`），隐藏输入新密钥。
  新密钥必须仍属于同一来源，不能借重配把现有队列切换给另一来源或另一中枢。
- **旧服务先迁移，再轮换**：旧版自动生成的服务文件可被严格识别并迁移；先用尚有效的旧密钥
  完成迁移和来源身份记录，再在 App 轮换。若旧密钥已失效、存在自定义 unit/drop-in 或未知配置，
  安装器停止覆盖并要求人工核对；不要删除队列绕过身份检查。
- 自定义队列路径、额外 Agent 配置或改写过的服务文件不会被自动重写。来源已有指标而本机
  没有原队列数据库时也会停止安装，避免序号归零后被中枢当成重复数据；应恢复原队列，或为新机器创建独立来源。

## 排障与退出状态

安装器成功返回 0；“服务已安装，但 120 秒内尚未确认指标”返回 3；配置、校验或安装失败返回非零。
遇到待确认上报，服务继续运行；检查以下内容，不要反复删除来源或重建队列：

```bash
systemctl is-active assistant-agent
systemctl status assistant-agent --no-pager
sudo journalctl -u assistant-agent -n 80 --no-pager
curl -fsS https://hub.example.com/api/v1/health
```

`health` 只确认中枢可达；App 来源中新的最后上报时间、版本和指标才是实际接入证据。

| 情况 | 处理 |
|---|---|
| 下载 502 / `agent_release_unavailable` | 检查选定版本的 Agent 清单和二进制是否已发布、中枢访问 GitHub/CDN、缓存磁盘空间与权限 |
| 401 | 来源密钥未知、已轮换或已删除；核对来源并按轮换步骤重配，不把账号登录令牌当来源密钥 |
| 403 | 来源被停用，或误用了 Feedback 来源；在 App 检查来源类型与启用状态 |
| 摘要 / 大小 / 版本不匹配 | 保留旧程序，排查资产与代理；不要跳过校验或覆盖已发布同版本资产 |
| 无 systemd / 架构不支持 | 首版安装器不适用；不要把宿主机安装器放进普通容器代替宿主机采集 |
| 服务运行但超时未确认 | 检查网络、代理、时间、队列及日志；较长上报周期可能超过 120 秒，稍后从 App 确认 |
| 自定义服务 / 来源身份不明 | 人工比较已有配置与服务，保留配置及队列，先解决差异再迁移 |

## 验证记录

本次源码、自动化、真实 Linux systemd、公开发布与生产设备体验分别记录在
`agent-install-validation.md`。只有真实服务器与手机效果由用户确认后才记“已验收”。
