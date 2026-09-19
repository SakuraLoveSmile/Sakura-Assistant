package com.sakurasep.assistant

import com.sakurasep.assistant.notify.FaultDebounce
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class FaultDebounceTest {

    @Test
    fun `allows up to 3 posts per 60s per faultId`() {
        var now = 1_000_000L
        val d = FaultDebounce(windowMs = 60_000, maxPerWindow = 3) { now }

        assertTrue(d.allow("flt_1"))
        assertTrue(d.allow("flt_1"))
        assertTrue(d.allow("flt_1"))
        assertFalse(d.allow("flt_1")) // 第 4 次被去抖

        // 不同 faultId 互不影响
        assertTrue(d.allow("flt_2"))

        // 窗口滑动后恢复
        now += 61_000
        assertTrue(d.allow("flt_1"))
    }

    @Test
    fun `sliding window expires entries`() {
        var now = 0L
        val d = FaultDebounce(windowMs = 60_000, maxPerWindow = 3) { now }
        repeat(3) {
            now += 20_000 // t=20s,40s,60s 各一次
            assertTrue(d.allow("flt_x"))
        }
        now += 21_000 // t=81s：第一次（20s）已滑出窗口
        assertTrue(d.allow("flt_x"))
    }

    @Test
    fun `reset clears fault`() {
        var now = 0L
        val d = FaultDebounce { now }
        repeat(3) { d.allow("flt_9") }
        assertFalse(d.allow("flt_9"))
        d.reset("flt_9")
        assertTrue(d.allow("flt_9"))
    }
}
