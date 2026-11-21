package com.wb2api.checkin.data

interface EngineClient {
    fun listAccounts(): List<Account>
    fun importAccount(content: String): Account
    fun checkin(uid: String): CheckinResult
}
