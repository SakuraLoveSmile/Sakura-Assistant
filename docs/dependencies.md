# 依赖清单（MVP 基线）

原则：版本固定、不用 `latest` / 浮动区间；新依赖优先选发布 ≥7 天的版本；
每个模块只引入下表范围内或论证后新增的依赖（新增须在 PR 说明理由）。

## 开发机工具链（已验证）

| 工具 | 版本 | 位置 / 说明 |
|---|---|---|
| Go | 1.26.5 | `/opt/homebrew/bin/go` |
| Flutter / Dart | 3.41.3 / 3.11.1 | `~/flutter/bin`（不在 PATH） |
| Android SDK | platforms 34–37，build-tools 35/36 | `~/Library/Android/sdk` |
| JDK | openjdk@17（Homebrew） | Gradle 构建用 `JAVA_HOME=/opt/homebrew/opt/openjdk@17` |
| adb | 已装 | 小米 10（umi）实测设备 |
| node / pnpm | 22 / 已装 | 仅 Feedback 仓联调用 |

## hub/（Go module `assistant-hub`）

| 依赖 | 版本约束 | 用途 |
|---|---|---|
| Go | ≥1.24（工具链 1.26） | — |
| `modernc.org/sqlite` | 最新稳定（纯 Go，免 CGO） | 存储 |
| 标准库 `net/http` | — | HTTP/SSE（不引框架，保持小依赖面） |
| `github.com/oklog/ulid/v2` | 最新稳定 | ID 生成 |

允许再议：仅当标准库确不够用时可引入一个微型 router（如 `chi`），PR 说明。

## agent/（Go module `assistant-agent`）

| 依赖 | 版本约束 | 用途 |
|---|---|---|
| Go | ≥1.24 | — |
| `modernc.org/sqlite` | 最新稳定 | 持久队列 |
| `github.com/shirou/gopsutil/v4` | 最新稳定 | CPU/内存/磁盘/网络/启动时间（Linux procfs） |
| 标准库 | — | docker API（经 `/var/run/docker.sock` HTTP）、smartctl 子进程 |

不引 vendor 之外的 NAS 专用 SDK：飞牛指标走 smartctl / lsblk / procfs /（如有）fnOS CLI 探测，
不支持的项按契约上报 `unsupported`。

## app/（Flutter）

| 依赖 | 版本约束 | 用途 |
|---|---|---|
| Flutter SDK | 3.41.3（Dart 3.11.1） | — |
| `dio` 或 `http` | 最新稳定 | REST/SSE（SSE 用 `http` 流式即可，避免重依赖） |
| `riverpod` 或 `provider` | 最新稳定 | 状态管理（二选一，定型后不混用） |
| `sqflite` / `drift` | 最新稳定 | Dart 侧历史缓存 |
| `flutter_secure_storage` | 最新稳定 | token 存放（Keystore 后端） |
| `go_router` | 最新稳定 | 路由 + 通知跳转 |
| `connectivity_plus` | 最新稳定 | 手机断网 vs 中枢不可达区分 |
| `intl` + `flutter_localizations` | SDK 配套 | 中文 UI / 时间格式 |

dev：`flutter_test`、`mocktail`（或 `mockito`）、`golden_toolkit`（可选）。

## app/android/（Kotlin）

| 依赖 | 版本约束 | 用途 |
|---|---|---|
| Kotlin | 2.x（AGP 兼容版） | — |
| AGP / Gradle | 8.x / wrapper 固定 | — |
| compileSdk / targetSdk / minSdk | 36 / 36 / 29 | 契约冻结 |
| OkHttp | 4.12.x | SSE 流式 + REST（原生侧） |
| Room | 2.6.x | `assistant_native.db` |
| WorkManager | 2.9.x | 断线兜底周期任务 |
| `androidx.core` / `appcompat` 或直接 `ComponentActivity` | 稳定版 | 通知、服务 |
| Kotlinx serialization 或 org.json | — | 载荷解析（轻量优先 org.json） |

构建：`./gradlew :app:assembleDebug` / `:app:assembleRelease`；JAVA_HOME 指 openjdk@17。

## deploy/

| 组件 | 说明 |
|---|---|
| `compose.hub.yml` | 中枢单容器（SQLite 卷）；Caddy 反代示例（TLS 自动签发） |
| `hub.env.example` | `ASSIST_ADMIN_USER/PASSWORD`/`ASSIST_DB_PATH`/`ASSIST_BASE_URL` 等样例 |
| agent 安装 | systemd unit + 二进制（T5 出安装脚本；NAS 侧视 fnOS 支持用 systemd 或开机任务） |

## 数据保留（契约值）

原始指标 7 天；5 分钟汇总 90 天；已结束消息 90 天；未恢复故障永久。变更窗口 ≥7 天 / 100k seq。
