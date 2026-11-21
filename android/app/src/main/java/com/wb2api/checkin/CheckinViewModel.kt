package com.wb2api.checkin

import android.content.Context
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.initializer
import androidx.lifecycle.viewmodel.viewModelFactory
import com.wb2api.checkin.data.Account
import com.wb2api.checkin.data.CheckinResult
import com.wb2api.checkin.data.EngineClient
import com.wb2api.checkin.data.GoEngineClient
import com.wb2api.checkin.data.ImportFile
import com.wb2api.checkin.data.ImportSummary
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import java.io.File
import java.util.concurrent.atomic.AtomicBoolean

data class AccountRow(
    val account: Account,
    val result: CheckinResult? = null,
)

data class UiState(
    val loading: Boolean = true,
    val rows: List<AccountRow> = emptyList(),
    val busy: Boolean = false,
    val progressDone: Int = 0,
    val progressTotal: Int = 0,
    val importSummary: ImportSummary? = null,
    val error: String? = null,
)

class CheckinViewModel(
    private val engine: EngineClient,
    private val io: CoroutineDispatcher = Dispatchers.IO,
) : ViewModel() {

    private val _state = MutableStateFlow(UiState())
    val state: StateFlow<UiState> = _state.asStateFlow()

    private val opLock = AtomicBoolean(false)

    private fun claimOperation(): Boolean {
        if (!opLock.compareAndSet(false, true)) return false
        _state.update { it.copy(busy = true) }
        return true
    }

    private fun releaseOperation() {
        _state.update { it.copy(busy = false) }
        opLock.set(false)
    }

    init {
        refresh()
    }

    fun refresh() {
        viewModelScope.launch(io) {
            _state.update { it.copy(loading = true, error = null) }
            runCatching { engine.listAccounts() }
                .onSuccess { accounts ->
                    _state.update { s ->
                        s.copy(loading = false, rows = mergeRows(accounts, s.rows))
                    }
                }
                .onFailure { e ->
                    _state.update {
                        it.copy(loading = false, error = "读取账号失败：${e.message}")
                    }
                }
        }
    }

    fun importFiles(files: List<ImportFile>) {
        if (files.isEmpty()) return
        if (!claimOperation()) {
            _state.update { it.copy(error = "正在处理中，请稍后再导入") }
            return
        }
        viewModelScope.launch(io) {
            try {
                _state.update { it.copy(error = null) }
                var ok = 0
                val failures = mutableListOf<String>()
                for (file in files) {
                    val content = file.content
                    if (content == null) {
                        failures += "${file.name}：无法读取文件"
                        continue
                    }
                    runCatching { engine.importAccount(content) }
                        .onSuccess { ok++ }
                        .onFailure { failures += "${file.name}：${it.message ?: "导入失败"}" }
                }
                _state.update {
                    it.copy(importSummary = ImportSummary(files.size, ok, failures))
                }
                runCatching { engine.listAccounts() }
                    .onSuccess { accounts ->
                        _state.update { s -> s.copy(rows = mergeRows(accounts, s.rows)) }
                    }
                    .onFailure { e ->
                        _state.update { it.copy(error = "刷新账号失败：${e.message}") }
                    }
            } finally {
                releaseOperation()
            }
        }
    }

    fun checkin(uid: String) {
        if (!claimOperation()) return
        viewModelScope.launch(io) {
            try {
                val result = runCheckin(uid)
                _state.update { s -> s.copy(rows = s.rows.merge(result)) }
            } finally {
                releaseOperation()
            }
        }
    }

    fun checkinAll() {
        if (!claimOperation()) return
        val targets = _state.value.rows.map { it.account.uid }
        if (targets.isEmpty()) {
            releaseOperation()
            return
        }
        viewModelScope.launch(io) {
            try {
                _state.update {
                    it.copy(progressDone = 0, progressTotal = targets.size)
                }
                targets.forEachIndexed { index, uid ->
                    val result = runCheckin(uid)
                    _state.update { s ->
                        s.copy(rows = s.rows.merge(result), progressDone = index + 1)
                    }
                }
            } finally {
                releaseOperation()
            }
        }
    }

    fun clearImportSummary() {
        _state.update { it.copy(importSummary = null) }
    }

    fun clearError() {
        _state.update { it.copy(error = null) }
    }

    private fun runCheckin(uid: String): CheckinResult =
        runCatching { engine.checkin(uid) }.getOrElse { e ->
            CheckinResult(
                uid = uid,
                status = "failed",
                message = "调用失败：${e.message ?: "未知错误"}",
            )
        }

    private fun mergeRows(accounts: List<Account>, old: List<AccountRow>): List<AccountRow> {
        val byUid = old.associateBy { it.account.uid }
        return accounts.map { AccountRow(it, byUid[it.uid]?.result) }
    }

    companion object {
        fun Factory(context: Context): ViewModelProvider.Factory = viewModelFactory {
            initializer {
                val app = context.applicationContext
                CheckinViewModel(GoEngineClient(File(app.filesDir, "auths")))
            }
        }
    }
}

private fun List<AccountRow>.merge(result: CheckinResult): List<AccountRow> =
    map { if (it.account.uid == result.uid) it.copy(result = result) else it }
