# T1 — 实机环境基线（小米 10 `8085d06e`）

采集时间：2026-09-18（`adb -s 8085d06e`）

## 设备与系统版本

| 项 | 值 | 来源 |
|---|---|---|
| product/model | `Mi 10`（umi） | `getprop ro.product.model` / `ro.product.device` |
| manufacturer | `Xiaomi` | `getprop ro.product.manufacturer` |
| Android | **16（SDK 36）** | `ro.build.version.release` / `ro.build.version.sdk` |
| HyperOS | **OS3.0.0.25.WOBCNXM**（`ro.mi.os.version.name=OS3.0`, `ro.mi.os.version.code=3`） | getprop |
| 旧 MIUI prop | `ro.miui.ui.version.name=V816` | getprop（HyperOS 保留旧字段） |
| build | `BP2A.250605.031.A3` | `ro.build.display.id` |

→ `getDeviceInfo` 的 `miui` 字段建议返回 `HyperOS 3.0.0.25`（`ro.mi.os.version.name`+code 优先，fallback `ro.miui.ui.version.name`）。

## 电池优化 / Doze 基线

- `dumpsys deviceidle whitelist`：user 白名单已含微信、QQ、小米系应用；**本应用未安装，无白名单项**。
- light idle：`light_after_inactive_to=+4m`, `light_idle_to=+5m`；deep idle：`idle_to=+1h`, `max_idle_to=+6h`。
- `mWakefulness=Dozing`（采集时屏幕关闭、设备在打盹）——正是验证 SSE 断线/Doze 行为的真实场景。
- MIUI 电池策略入口：`openBatteryOptimizationSettings` 走 `ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`；
  MIUI 另有「省电策略：无限制/智能限制」应用级设置（SecurityCenter 管），与 AOSP 白名单并存。

## 自启动权限

- MIUI 自启动由 `com.miui.securitycenter`（手机管家）+ AppOps `OP_AUTO_START`（op 10008，非公开 API）管理；
  `dumpsys appops` 未见导出可查询条目，`service list` 中有 `ProcessManager: miui.IProcessManager`（MIUI 私有服务，无公开调用）。
- **结论：自启动状态无法经 adb/dumpsys 可靠读取，只能跳转引导页让用户手动开**。
  `openAutostartSettings` 需多 intent fallback（见实现）。
- `dumpsys activity lru` 显示大量 BFGS 进程被 `treated`（MIUI 后台压制机制运行中）。

## 其他

- `com.sakurasep.assistant` 当前**未安装**；同用户另有 `dev.sakurasep.comic`。
- `settings get global device_idle_constants` = null（未自定义 Doze 参数）。
- 待机分桶（usagestats）显示应用闲置后 bucket→40/50 正常降级。
- 屏幕状态采集时 Dozing；通知测试需先 `input keyevent KEYCODE_WAKEUP` 或验证锁屏横幅。

## 对本实现的影响

1. HyperOS 3 = Android 16 基底：`compileSdk/targetSdk=36` 正确；前台服务 `dataSync` type 必须声明，
   `startForeground` 需在 `onStartCommand` 早段调用（超时 ANR/崩溃）。
2. 后台压制强：SSE 长连接被杀风险高 → WorkManager 15min 兜底 + BOOT_COMPLETED 拉起都不可省。
3. 自启动开关默认关：BootReceiver 拉起在「未开自启动」时大概率被 MIUI 拦截 → 需实测并如实记录。
4. `pm grant POST_NOTIFICATIONS` 可在安装后授权；用户日常需手动开「自启动 + 无限制省电」。
