package com.sakurasep.assistant

import com.sakurasep.assistant.bridge.NativeEvents
import java.time.Instant

/**
 * 服务运行状态（进程内单例；服务与 MainActivity 同进程，MethodChannel 直接读）。
 * 字段变更后调用 [emit] 推送 {type:"service"} 事件。
 */
object SyncState {
    @Volatile var running = false
    @Volatile var hubReachable = false
    @Volatile var lastSyncAtMs: Long? = null
    @Volatile var lastError: String? = null
    @Volatile var dndActive = false
    @Volatile var heldCount = 0
    /** 401 且 refresh 失败：凭证彻底失效，等待 Dart 重新登录后 configure。 */
    @Volatile var authExpired = false

    fun snapshot(lastChangeSeq: Long): Map<String, Any?> = mapOf(
        "running" to running,
        "hubReachable" to hubReachable,
        "lastSyncAt" to lastSyncAtMs?.let { Instant.ofEpochMilli(it).toString() },
        "lastChangeSeq" to lastChangeSeq,
        "dndActive" to dndActive,
        "heldCount" to heldCount,
        "authExpired" to authExpired,
        "lastError" to lastError,
    )

    /** 推送服务状态事件（bridge.md §2 type=service）。 */
    fun emit(lastChangeSeq: Long) {
        NativeEvents.emitServiceState(snapshot(lastChangeSeq))
    }
}
