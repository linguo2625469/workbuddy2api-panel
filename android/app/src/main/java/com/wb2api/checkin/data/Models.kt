package com.wb2api.checkin.data

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

@Serializable
data class Account(
    val uid: String,
    val nickname: String = "",
    val realm: String = "cn",
    val enterprise: Boolean = false,
    val file: String = "",
)

@Serializable
data class CheckinResult(
    val uid: String,
    val nickname: String = "",
    val status: String,
    val message: String = "",
    @SerialName("checked_at") val checkedAt: String = "",
)

data class ImportFile(val name: String, val content: String?)

data class ImportSummary(
    val total: Int,
    val ok: Int,
    val failures: List<String>,
)
