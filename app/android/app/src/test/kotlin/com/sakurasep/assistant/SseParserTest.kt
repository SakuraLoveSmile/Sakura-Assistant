package com.sakurasep.assistant

import com.sakurasep.assistant.sse.SseParser
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class SseParserTest {

    private fun feedLines(parser: SseParser, block: String): List<SseParser.Event> =
        block.lines().mapNotNull { parser.feed(it) }

    @Test
    fun `parses event data id`() {
        val p = SseParser()
        val events = feedLines(
            p,
            "id: 42\nevent: message\ndata: {\"id\":\"msg_1\"}\n\n",
        )
        assertEquals(1, events.size)
        assertEquals("message", events[0].type)
        assertEquals("{\"id\":\"msg_1\"}", events[0].data)
        assertEquals("42", events[0].id)
    }

    @Test
    fun `default event type is message`() {
        val p = SseParser()
        val events = feedLines(p, "data: {\"x\":1}\n\n")
        assertEquals("message", events[0].type)
    }

    @Test
    fun `multiline data joins with newline`() {
        val p = SseParser()
        val events = feedLines(p, "data: line1\ndata: line2\n\n")
        assertEquals("line1\nline2", events[0].data)
    }

    @Test
    fun `comment and ping-like lines ignored`() {
        val p = SseParser()
        val events = feedLines(p, ": keep-alive\n\nevent: ping\ndata: {}\n\n")
        assertEquals(1, events.size)
        assertEquals("ping", events[0].type)
    }

    @Test
    fun `id persists across events until overwritten`() {
        val p = SseParser()
        feedLines(p, "id: 7\ndata: a\n\n")
        val second = feedLines(p, "data: b\n\n")
        assertEquals("7", second[0].id)
    }

    @Test
    fun `data-less block produces no event`() {
        val p = SseParser()
        val events = feedLines(p, "event: ping\n\n")
        assertEquals(0, events.size)
    }
}
