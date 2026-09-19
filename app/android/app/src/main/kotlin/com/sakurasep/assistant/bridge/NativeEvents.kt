package com.sakurasep.assistant.bridge

import android.os.Handler
import android.os.Looper
import com.sakurasep.assistant.util.Dbg
import io.flutter.plugin.common.EventChannel

/**
 * EventChannel `assistant/native_events` 的原生侧出口（bridge.md §2）。
 * Dart 存活期间投递 launch / service / summary；sink 缺失时事件静默丢弃
 * （launch 已由 pending route 兜底）。
 */
object NativeEvents {
    private val mainHandler = Handler(Looper.getMainLooper())

    @Volatile
    var sink: EventChannel.EventSink? = null
        private set

    fun attach(s: EventChannel.EventSink?) {
        sink = s
        Dbg.i("NativeEvents sink ${if (s != null) "attached" else "detached"}")
    }

    fun emit(map: Map<String, Any?>) {
        val s = sink ?: run {
            Dbg.timed("NativeEvents.emit dropped (no sink): $map")
            return
        }
        if (Looper.myLooper() == Looper.getMainLooper()) {
            runCatching { s.success(map) }
        } else {
            mainHandler.post { runCatching { s.success(map) } }
        }
    }

    fun emitLaunch(route: String, id: String?) {
        emit(mapOf("type" to "launch", "route" to route, "id" to id))
    }

    fun emitServiceState(state: Map<String, Any?>) {
        emit(mapOf("type" to "service", "state" to state))
    }

    fun emitSummary(delivered: Int) {
        emit(mapOf("type" to "summary", "delivered" to delivered))
    }

    /** 401 自愈轮换后的凭证回写：Dart 侧据此同步刷新会话（refreshToken 一次性失效旧值）。 */
    fun emitSession(token: String, refreshToken: String) {
        emit(mapOf("type" to "session", "token" to token, "refreshToken" to refreshToken))
    }
}
