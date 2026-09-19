package com.sakurasep.assistant.bridge

import android.app.Activity
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.provider.Settings
import com.sakurasep.assistant.AssistantSyncService
import com.sakurasep.assistant.SyncState
import com.sakurasep.assistant.data.ConfigStore
import com.sakurasep.assistant.dnd.DndWindow
import com.sakurasep.assistant.data.NativeDb
import com.sakurasep.assistant.util.Dbg
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch

/**
 * MethodChannel `assistant/native` 全部方法（bridge.md §1）。
 * Room 读写走 IO 协程，结果回主线程回调。
 */
object NativeBridge {
    private val mainHandler = Handler(Looper.getMainLooper())
    private val ioScope = CoroutineScope(Dispatchers.IO)

    fun handle(activity: Activity, call: MethodCall, result: MethodChannel.Result) {
        when (call.method) {
            "configure" -> configure(activity, call, result)
            "startService" -> startService(activity, result)
            "stopService" -> stopService(activity, result)
            "getServiceState" -> getServiceState(activity, result)
            "getSession" -> getSession(activity, result)
            "getLaunchPayload", "consumePendingRoute" -> consumePendingRoute(activity, result)
            "openNotificationSettings" ->
                result.success(openNotificationSettings(activity))
            "openBatteryOptimizationSettings" ->
                result.success(openBatteryOptimizationSettings(activity))
            "openAutostartSettings" ->
                result.success(openAutostartSettings(activity))
            "getDeviceInfo" -> result.success(deviceInfo())
            "setDebugLogging" -> setDebugLogging(activity, call, result)
            else -> result.notImplemented()
        }
    }

    private fun ok(result: MethodChannel.Result, value: Any?) {
        if (Looper.myLooper() == Looper.getMainLooper()) result.success(value)
        else mainHandler.post { result.success(value) }
    }

    // ---------------- 配置 ----------------

    private fun configure(activity: Activity, call: MethodCall, result: MethodChannel.Result) {
        val hubUrl = call.argument<String>("hubUrl") ?: ""
        val token = call.argument<String>("token") ?: ""
        val refreshToken = call.argument<String>("refreshToken")
        val interval = call.argument<Number>("reportIntervalSeconds")?.toInt() ?: 30
        val dnd = call.argument<Map<String, Any?>>("dnd")
        ioScope.launch {
            val store = ConfigStore(activity)
            val before = store.load()
            val cfg = store.update { c ->
                c.copy(
                    hubUrl = hubUrl,
                    token = token,
                    refreshToken = refreshToken ?: c.refreshToken,
                    reportIntervalSeconds = interval,
                    dndEnabled = dnd?.get("enabled") as? Boolean ?: c.dndEnabled,
                    dndStart = dnd?.get("start") as? String ?: c.dndStart,
                    dndEnd = dnd?.get("end") as? String ?: c.dndEnd,
                    dndTimezone = dnd?.get("timezone") as? String ?: c.dndTimezone,
                )
            }
            // token/hubUrl 变化 → 重建 SSE；dnd 变化 → 立即重算抑制状态
            if (before.hubUrl != cfg.hubUrl || before.token != cfg.token) {
                SyncState.authExpired = false
                Dbg.i("configure: credentials changed → reconnect SSE")
                if (SyncState.running) {
                    runCatching { AssistantSyncService.requestReconnect(activity) }
                }
            }
            if (dndChanged(before, cfg)) {
                Dbg.i("configure: dnd changed")
                if (SyncState.running) {
                    runCatching { AssistantSyncService.requestReconnect(activity) }
                }
            }
            ok(result, mapOf("ok" to true))
        }
    }

    private fun dndChanged(a: com.sakurasep.assistant.data.ConfigEntity, b: com.sakurasep.assistant.data.ConfigEntity) =
        a.dndEnabled != b.dndEnabled || a.dndStart != b.dndStart ||
            a.dndEnd != b.dndEnd || a.dndTimezone != b.dndTimezone

    // ---------------- 服务启停 ----------------

    private fun startService(activity: Activity, result: MethodChannel.Result) {
        ioScope.launch {
            ConfigStore(activity).update { it.copy(serviceRequested = true) }
            runCatching { AssistantSyncService.start(activity) }
                .onFailure { Dbg.w("startService failed", it) }
            ok(result, mapOf("running" to true))
        }
    }

    private fun stopService(activity: Activity, result: MethodChannel.Result) {
        ioScope.launch {
            ConfigStore(activity).update { it.copy(serviceRequested = false) }
            runCatching { AssistantSyncService.requestStop(activity) }
            ok(result, mapOf("running" to false))
        }
    }

    // ---------------- 状态 ----------------

    private fun getServiceState(activity: Activity, result: MethodChannel.Result) {
        ioScope.launch {
            val db = NativeDb.get(activity)
            val cfg = db.configDao().get()
            SyncState.dndActive = DndWindow.isActiveAt(
                System.currentTimeMillis(),
                cfg?.dndEnabled == true,
                cfg?.dndStart ?: "23:00",
                cfg?.dndEnd ?: "08:00",
                cfg?.dndTimezone ?: "Asia/Shanghai",
            )
            SyncState.heldCount = db.heldDao().count()
            ok(result, SyncState.snapshot(cfg?.lastChangeSeq ?: 0))
        }
    }

    /** 原生侧当前持有的凭证；refreshToken 一次性轮换，Dart 冷启动 / refresh 失败时据此对齐。 */
    private fun getSession(activity: Activity, result: MethodChannel.Result) {
        ioScope.launch {
            val cfg = ConfigStore(activity).load()
            ok(
                result,
                if (cfg.token.isBlank()) null
                else mapOf("token" to cfg.token, "refreshToken" to cfg.refreshToken),
            )
        }
    }

    /** getLaunchPayload / consumePendingRoute：同一 pending route，取一次即清除。 */
    private fun consumePendingRoute(activity: Activity, result: MethodChannel.Result) {
        ioScope.launch {
            val (route, id) = ConfigStore(activity).consumePendingRoute()
            ok(
                result,
                route?.let { mapOf("route" to it, "id" to id) },
            )
        }
    }

    // ---------------- 设置页跳转 ----------------

    private fun tryStart(activity: Activity, intent: Intent): Boolean =
        runCatching {
            intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            if (intent.resolveActivity(activity.packageManager) != null ||
                activity.packageManager.queryIntentActivities(
                    intent, PackageManager.MATCH_ALL,
                ).isNotEmpty()
            ) {
                activity.startActivity(intent); true
            } else false
        }.getOrDefault(false)

    private fun openNotificationSettings(activity: Activity): Boolean {
        val pkg = activity.packageName
        return tryStart(
            activity,
            Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                .putExtra(Settings.EXTRA_APP_PACKAGE, pkg),
        ) || tryStart(
            activity,
            Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
                .setData(Uri.parse("package:$pkg")),
        )
    }

    private fun openBatteryOptimizationSettings(activity: Activity): Boolean {
        val pkg = activity.packageName
        return tryStart(
            activity,
            Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS)
                .setData(Uri.parse("package:$pkg")),
        ) || tryStart(activity, Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS))
    }

    /** 尽力跳 MIUI 自启管理页（多 intent fallback；不可达返回 false）。 */
    private fun openAutostartSettings(activity: Activity): Boolean {
        val pkg = activity.packageName
        val candidates = listOf(
            // MIUI 安全中心自启动管理（各版本类名兜底）
            Intent().setClassName(
                "com.miui.securitycenter",
                "com.miui.permcenter.autostart.AutoStartManagementActivity",
            ),
            Intent("miui.intent.action.OP_AUTO_START").addCategory(Intent.CATEGORY_DEFAULT),
            Intent().setClassName(
                "com.miui.securitycenter",
                "com.miui.powercenter.PowerSettings",
            ),
            Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
                .setData(Uri.parse("package:$pkg")),
        )
        for (i in candidates) {
            if (tryStart(activity, i)) return true
        }
        return false
    }

    // ---------------- 设备信息 ----------------

    private fun deviceInfo(): Map<String, Any?> {
        val miui = runCatching {
            val osName = getProp("ro.mi.os.version.name")
            if (!osName.isNullOrEmpty()) {
                "HyperOS ${osName.removePrefix("OS")}" +
                    (getProp("ro.mi.os.version.incremental")?.let { " ($it)" } ?: "")
            } else getProp("ro.miui.ui.version.name")
        }.getOrNull()
        return mapOf(
            "manufacturer" to Build.MANUFACTURER,
            "model" to Build.MODEL,
            "sdkInt" to Build.VERSION.SDK_INT,
            "miui" to miui,
        )
    }

    private fun getProp(key: String): String? = runCatching {
        val process = Runtime.getRuntime().exec(arrayOf("getprop", key))
        process.inputStream.bufferedReader().readText().trim().ifEmpty { null }
    }.getOrNull()

    // ---------------- 调试日志 ----------------

    private fun setDebugLogging(activity: Activity, call: MethodCall, result: MethodChannel.Result) {
        val enabled = call.argument<Boolean>("enabled") == true
        Dbg.enabled = enabled
        ioScope.launch {
            ConfigStore(activity).update { it.copy(debugLogging = enabled) }
            ok(result, mapOf("ok" to true))
        }
    }
}
