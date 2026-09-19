package com.sakurasep.assistant

import android.content.Intent
import android.os.Bundle
import com.sakurasep.assistant.bridge.NativeBridge
import com.sakurasep.assistant.bridge.NativeEvents
import com.sakurasep.assistant.data.ConfigStore
import com.sakurasep.assistant.notify.NotificationHelper
import com.sakurasep.assistant.util.Dbg
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.EventChannel
import io.flutter.plugin.common.MethodChannel
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch

/**
 * MethodChannel `assistant/native` + EventChannel `assistant/native_events` 宿主。
 * 通知点击经 extra(route,id) 到达 → 先写 pending route，再发 launch 事件（bridge.md §2/§3）。
 */
class MainActivity : FlutterActivity() {

    private val ioScope = CoroutineScope(Dispatchers.IO)

    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        Dbg.i("MainActivity configureFlutterEngine")

        MethodChannel(
            flutterEngine.dartExecutor.binaryMessenger,
            "assistant/native",
        ).setMethodCallHandler { call, result ->
            runCatching { NativeBridge.handle(this, call, result) }
                .onFailure {
                    Dbg.w("method ${call.method} failed", it)
                    result.error("native_error", it.message, null)
                }
        }

        EventChannel(
            flutterEngine.dartExecutor.binaryMessenger,
            "assistant/native_events",
        ).setStreamHandler(object : EventChannel.StreamHandler {
            override fun onListen(arguments: Any?, events: EventChannel.EventSink?) {
                NativeEvents.attach(events)
            }

            override fun onCancel(arguments: Any?) {
                NativeEvents.attach(null)
            }
        })

        // 进程可能因通知点击冷启动而 Dart 尚未就绪：恢复持久化的 debugLogging 开关
        ioScope.launch {
            Dbg.enabled = ConfigStore(this@MainActivity).load().debugLogging
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        handleRouteIntent(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handleRouteIntent(intent)
    }

    /** extra(route,id) → pending route 暂存 + launch 事件；事件丢失由 consumePendingRoute 兜底。 */
    private fun handleRouteIntent(intent: Intent?) {
        val route = intent?.getStringExtra(NotificationHelper.EXTRA_ROUTE) ?: return
        val id = intent.getStringExtra(NotificationHelper.EXTRA_ID)
        Dbg.i("route intent: route=$route id=$id")
        ioScope.launch {
            ConfigStore(this@MainActivity).setPendingRoute(route, id)
            NativeEvents.emitLaunch(route, id)
        }
    }
}
