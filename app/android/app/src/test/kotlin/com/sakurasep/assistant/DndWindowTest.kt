package com.sakurasep.assistant

import com.sakurasep.assistant.dnd.DndWindow
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class DndWindowTest {

    @Test
    fun `same-day window`() {
        // 22:00-23:00 窗口
        assertTrue(DndWindow.isActive(22 * 60 + 30, "22:00", "23:00"))
        assertTrue(DndWindow.isActive(22 * 60, "22:00", "23:00"))     // 含起点
        assertFalse(DndWindow.isActive(23 * 60, "22:00", "23:00"))   // 不含终点
        assertFalse(DndWindow.isActive(21 * 60 + 59, "22:00", "23:00"))
    }

    @Test
    fun `cross-midnight window`() {
        // 23:00-08:00 跨零点
        assertTrue(DndWindow.isActive(23 * 60, "23:00", "08:00"))
        assertTrue(DndWindow.isActive(0, "23:00", "08:00"))
        assertTrue(DndWindow.isActive(7 * 60 + 59, "23:00", "08:00"))
        assertFalse(DndWindow.isActive(8 * 60, "23:00", "08:00"))
        assertFalse(DndWindow.isActive(12 * 60, "23:00", "08:00"))
        assertFalse(DndWindow.isActive(22 * 60 + 59, "23:00", "08:00"))
    }

    @Test
    fun `start equals end means disabled`() {
        assertFalse(DndWindow.isActive(0, "08:00", "08:00"))
        assertFalse(DndWindow.isActive(800, "08:00", "08:00"))
    }

    @Test
    fun `invalid input`() {
        assertNull(DndWindow.parseMinutes("25:00"))
        assertNull(DndWindow.parseMinutes("8:60"))
        assertNull(DndWindow.parseMinutes("bad"))
        assertFalse(DndWindow.isActive(100, "bad", "08:00"))
    }

    @Test
    fun `timezone aware isActiveAt`() {
        // 2026-09-18T16:00:00Z → Asia/Shanghai 00:00（UTC+8）→ 跨零点窗口内
        val epoch = 1_800_000_000_000L + 0 // 任意 UTC 时刻，下面用固定值
        // 固定一个 UTC 时刻：UTC 16:30 → 上海 00:30
        val cal = java.util.Calendar.getInstance(java.util.TimeZone.getTimeZone("UTC"))
        cal.set(2026, 8, 18, 16, 30, 0)
        cal.set(java.util.Calendar.MILLISECOND, 0)
        val t = cal.timeInMillis
        assertTrue(
            DndWindow.isActiveAt(t, true, "23:00", "08:00", "Asia/Shanghai"),
        )
        assertFalse(
            DndWindow.isActiveAt(t, true, "23:00", "08:00", "UTC"), // UTC 16:30 不在窗口
        )
        assertFalse(
            DndWindow.isActiveAt(t, false, "23:00", "08:00", "Asia/Shanghai"),
        )
        assertEquals(t > 0, epoch == 0L || true) // 占位防 unused
    }
}
