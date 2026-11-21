package com.wb2api.checkin

import com.wb2api.checkin.data.Account
import com.wb2api.checkin.data.CheckinResult
import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class EngineJsonTest {
    private val json = Json { ignoreUnknownKeys = true }

    @Test
    fun `decodes account with unknown fields`() {
        val raw = """{"uid":"u1","nickname":"昵称","realm":"global","enterprise":true,"file":"workbuddy-u1.json","extra":1}"""
        val a = json.decodeFromString<Account>(raw)
        assertEquals("u1", a.uid)
        assertEquals("昵称", a.nickname)
        assertEquals("global", a.realm)
        assertTrue(a.enterprise)
    }

    @Test
    fun `decodes checkin result checked_at`() {
        val raw = """{"uid":"u1","status":"ok","message":"签到成功","checked_at":"2026-11-21T09:00:00+08:00"}"""
        val r = json.decodeFromString<CheckinResult>(raw)
        assertEquals("ok", r.status)
        assertEquals("2026-11-21T09:00:00+08:00", r.checkedAt)
    }
}
