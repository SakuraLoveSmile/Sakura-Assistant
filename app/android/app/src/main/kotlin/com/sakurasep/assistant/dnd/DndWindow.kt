package com.sakurasep.assistant.dnd

import java.util.Calendar
import java.util.TimeZone

/**
 * DND 窗口判定（bridge.md §3）：HH:mm 本地窗口，支持跨零点（如 23:00–08:00）。
 * 纯逻辑类，不依赖 Android —— 可单测。
 */
object DndWindow {

    /**
     * @param minutesOfDay 当前时刻在窗口时区下的分钟数（0..1439）
     * @param start "HH:mm"
     * @param end "HH:mm"
     * @return 窗口 [start, end) 内返回 true；start == end 视为不启用窗口（恒 false）。
     */
    fun isActive(minutesOfDay: Int, start: String, end: String): Boolean {
        val s = parseMinutes(start) ?: return false
        val e = parseMinutes(end) ?: return false
        if (s == e) return false
        return if (s < e) {
            minutesOfDay in s until e
        } else {
            // 跨零点：>= start 或 < end
            minutesOfDay >= s || minutesOfDay < e
        }
    }

    fun isActiveAt(epochMillis: Long, enabled: Boolean, start: String, end: String, timezone: String): Boolean {
        if (!enabled) return false
        val tz = runCatching { TimeZone.getTimeZone(timezone) }.getOrNull()
            ?: TimeZone.getDefault()
        val cal = Calendar.getInstance(tz).apply { timeInMillis = epochMillis }
        val minutes = cal.get(Calendar.HOUR_OF_DAY) * 60 + cal.get(Calendar.MINUTE)
        return isActive(minutes, start, end)
    }

    /** 解析 "HH:mm" → 分钟数（0..1439）；非法返回 null。 */
    fun parseMinutes(hhmm: String): Int? {
        val parts = hhmm.trim().split(":")
        if (parts.size != 2) return null
        val h = parts[0].toIntOrNull() ?: return null
        val m = parts[1].toIntOrNull() ?: return null
        if (h !in 0..23 || m !in 0..59) return null
        return h * 60 + m
    }

    /**
     * 距窗口结束的毫秒数（用于调度 flush 检查；窗口未激活返回 null）。
     * 简化：返回下一个 end 时刻与 now 的差值。
     */
    fun millisUntilEnd(epochMillis: Long, start: String, end: String, timezone: String): Long? {
        val e = parseMinutes(end) ?: return null
        val tz = runCatching { TimeZone.getTimeZone(timezone) }.getOrNull() ?: TimeZone.getDefault()
        val cal = Calendar.getInstance(tz).apply {
            timeInMillis = epochMillis
            set(Calendar.SECOND, 0)
            set(Calendar.MILLISECOND, 0)
        }
        val nowMin = cal.get(Calendar.HOUR_OF_DAY) * 60 + cal.get(Calendar.MINUTE)
        var diffMin = e - nowMin
        if (diffMin <= 0) diffMin += 24 * 60
        return diffMin * 60_000L - (epochMillis - cal.timeInMillis)
    }
}
