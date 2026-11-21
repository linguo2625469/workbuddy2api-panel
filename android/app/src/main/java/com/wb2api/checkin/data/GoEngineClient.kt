package com.wb2api.checkin.data

import com.wb2api.engine.mobile.Engine
import com.wb2api.engine.mobile.Mobile
import kotlinx.serialization.json.Json
import java.io.File

// 注意：gomobile 会在 -javapkg 后再拼接 Go 包名，实际 Java 包是 com.wb2api.engine.mobile。
class GoEngineClient(authDir: File) : EngineClient {
    private val engine: Engine = Mobile.newEngine(authDir.absolutePath)
    private val json = Json { ignoreUnknownKeys = true }

    override fun listAccounts(): List<Account> =
        json.decodeFromString(engine.listAccounts())

    override fun importAccount(content: String): Account =
        json.decodeFromString(engine.importAccount(content))

    override fun checkin(uid: String): CheckinResult =
        json.decodeFromString(engine.checkin(uid))
}
