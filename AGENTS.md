# Assistant — Agent 工作约定

个人助手 MVP：**Android APK（小米 10 / HyperOS 3）+ Go 中枢 + Go 采集 + SQLite**。
中枢部署在公网服务器，设备主动上报，NAS 不开公网端口。MVP 不含桌面端、不引入 Rust。

## 契约优先

`contracts/` 下四份文档已冻结，是所有模块的唯一接口依据：

| 文件 | 内容 |
|---|---|
| `contracts/api-v1.md` | 中枢 HTTP 契约：认证、接入（metrics/events）、客户端读取、管理、保留策略 |
| `contracts/events.md` | 事件种类、faultKey、故障生命周期、去重/乱序、规则引擎、来源队列要求 |
| `contracts/bridge.md` | Flutter↔Kotlin MethodChannel/EventChannel、通知行为、DND 执行、Manifest 基线 |
| `contracts/feedback-integration.md` | Feedback 侧 outbox、只读附件接口、环境变量、中枢回连约定 |
| `contracts/fixtures/` | 契约示例载荷（开发/测试可直接使用） |

只允许兼容变更（新端点、新可选字段、枚举新值）。破坏性变更必须开 `/api/v2` 并保留 v1。
改契约 = 主 agent 职责，执行者不得在模块内私自改语义。

## 目录与所有权（同一文件只归一个执行者）

| 路径 | 所有者 | 说明 |
|---|---|---|
| `contracts/`, `deploy/`, `docs/`, 根文件 | 主 agent | 契约、部署、依赖清单、集成文档 |
| `hub/` | 中枢 agent | Go module `assistant-hub`：认证、存储、规则引擎、SSE、附件代理 |
| `agent/` | 采集 agent | Go module `assistant-agent`：指标采集、NAS 探测、持久队列补传 |
| `app/lib/`, `app/test/`, `app/pubspec.yaml`, `app/analysis_options.yaml` | Flutter agent | Dart 页面、客户端状态、REST/通道封装、测试 |
| `app/android/` | Android agent | Kotlin 前台服务、通知、Manifest、Gradle 构建 |
| `../Feedback`（`assist` 相关增量文件） | Feedback agent | outbox 表、投递 worker、`/api/assist/*` 只读路由 |

## 环境（开发机已验证）

- Go 1.26.5（`/opt/homebrew/bin/go`）；Flutter 3.41.3 / Dart 3.11.1（`~/flutter/bin`，不在 PATH，用全路径或 `export PATH="$HOME/flutter/bin:$PATH"`）
- Android SDK：`~/Library/Android/sdk`（platforms 34–37、build-tools 35/36、cmdline-tools、ndk）；
  `adb` 在 PATH；**小米 10（umi）已连接**（`adb devices` 可见 `8085d06e`）
- JDK：Homebrew `openjdk@17`/`@21`（`/opt/homebrew/opt/openjdk@17`）；Gradle 用 `JAVA_HOME` 指向它们
- node 22 + pnpm（Feedback 仓测试用）；Docker CLI 在但守护进程常未启动（`open -a Docker` 或跳过容器验证）
- 部署目标是 Linux；采集程序须 `GOOS=linux` 交叉编译验证（amd64 服务器 + arm64 NAS 视型号而定）

## 验证要求

- 中枢：`cd hub && go test ./...`（去重、乱序、故障轮次、规则引擎、sync 一致性必须有测试）
- 采集：`cd agent && go test ./... && GOOS=linux GOARCH=amd64 go build ./...`（队列、退避、溢出必须测）
- Flutter：`cd app && ~/flutter/bin/flutter test` + `flutter analyze`
- Android：`cd app && ./gradlew :app:assembleDebug :app:testDebugUnitTest`（JAVA_HOME 指 openjdk@17）
- 不得以模拟结果代替实机验证：锁屏 / 划掉任务 / 重启 / DND / 隔夜只在小米 10 上验证并记录证据。
- 真实部署按届时授权执行；本地不得向公网服务器推送任何东西。

## 硬性规则

- 不提交密钥与令牌；`deploy/*.env` 只出 `.example`。不读、不记密码与 API key 明文。
- Feedback 仓当前有未提交修改：**只增量编辑，绝不执行 git 写操作**。
- 事件与指标宁可丢进持久队列也不静默丢失；队列必须有上限与溢出汇总。
- 缺失指标显示「不支持/采集失败」，绝不显示正常；SMART 探测不得唤醒休眠盘（`smartctl -n standby`）。
