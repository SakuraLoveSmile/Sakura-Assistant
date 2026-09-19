package com.sakurasep.assistant.notify

/**
 * 故障通知去抖（bridge.md §3）：同一 faultId 60s 内最多 3 次通知更新，防风暴刷屏。
 * 纯逻辑类，可单测。
 */
class FaultDebounce(
    private val windowMs: Long = 60_000L,
    private val maxPerWindow: Int = 3,
    private val now: () -> Long = System::currentTimeMillis,
) {
    private val hits = HashMap<String, ArrayDeque<Long>>()

    /** 返回 true 表示允许本次通知；false 表示窗口内已超限，应静默跳过。 */
    fun allow(faultId: String): Boolean {
        val t = now()
        val q = hits.getOrPut(faultId) { ArrayDeque() }
        while (q.isNotEmpty() && t - q.first() > windowMs) q.removeFirst()
        return if (q.size < maxPerWindow) {
            q.addLast(t)
            true
        } else {
            false
        }
    }

    fun reset(faultId: String) {
        hits.remove(faultId)
    }

    fun clear() = hits.clear()
}
