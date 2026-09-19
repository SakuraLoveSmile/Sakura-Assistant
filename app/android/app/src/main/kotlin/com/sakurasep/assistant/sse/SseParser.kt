package com.sakurasep.assistant.sse

/**
 * SSE 事件解析（api-v1.md §3 /api/v1/stream）：逐行喂入，空行触发一次事件。
 * 纯逻辑类，不依赖 OkHttp —— 可单测。
 */
class SseParser {

    data class Event(
        /** event: 字段；缺省按 SSE 规范为 "message"。 */
        val type: String,
        /** data: 多行以 \n 拼接。 */
        val data: String,
        /** id: 字段（changeSeq）；无则 null。 */
        val id: String?,
    )

    private var eventName: String? = null
    private val dataLines = StringBuilder()
    private var lastId: String? = null

    /**
     * 喂入一行（不含换行符）。返回 null 表示事件未完整；空行时返回一个 Event（无数据则无事件）。
     */
    fun feed(line: String): Event? {
        if (line.isEmpty()) {
            val ev = dispatch()
            return ev
        }
        if (line.startsWith(':')) return null // 注释/保活行
        val colon = line.indexOf(':')
        val field: String
        var value: String
        if (colon < 0) {
            field = line
            value = ""
        } else {
            field = line.substring(0, colon)
            value = line.substring(colon + 1)
            if (value.startsWith(" ")) value = value.substring(1)
        }
        when (field) {
            "event" -> eventName = value
            "data" -> {
                if (dataLines.isNotEmpty()) dataLines.append('\n')
                dataLines.append(value)
            }
            "id" -> if (!value.contains('�')) lastId = value // 含 �(U+FFFD) 按规范忽略
            "retry" -> { /* 自定义退避策略，不采用服务端 retry */ }
        }
        return null
    }

    private fun dispatch(): Event? {
        val data = dataLines.toString()
        val ev = if (data.isEmpty()) null else Event(eventName ?: "message", data, lastId)
        eventName = null
        dataLines.setLength(0)
        return ev
    }
}
