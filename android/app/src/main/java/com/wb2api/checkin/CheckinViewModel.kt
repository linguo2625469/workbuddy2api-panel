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
        if (files.isEmpty() || _state.value.busy) return
        viewModelScope.launch(io) {
            _state.update { it.copy(busy = true, error = null) }
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
                it.copy(
                    busy = false,
                    importSummary = ImportSummary(files.size, ok, failures),
                )
            }
            runCatching { engine.listAccounts() }
                .onSuccess { accounts ->
                    _state.update { s -> s.copy(rows = mergeRows(accounts, s.rows)) }
                }
                .onFailure { e ->
                    _state.update { it.copy(error = "刷新账号失败：${e.message}") }
                }
        }
    }

    fun checkin(uid: String) {
        if (_state.value.busy) return
        viewModelScope.launch(io) {
            _state.update { it.copy(busy = true) }
            val result = runCheckin(uid)
            _state.update { s ->
                s.copy(busy = false, rows = s.rows.merge(result))
            }
        }
    }

    fun checkinAll() {
        val snapshot = _state.value
        if (snapshot.busy || snapshot.rows.isEmpty()) return
        val targets = snapshot.rows.map { it.account.uid }
        viewModelScope.launch(io) {
            _state.update {
                it.copy(busy = true, progressDone = 0, progressTotal = targets.size)
            }
            targets.forEachIndexed { index, uid ->
                val result = runCheckin(uid)
                _state.update { s ->
                    s.copy(rows = s.rows.merge(result), progressDone = index + 1)
                }
            }
            _state.update { it.copy(busy = false) }
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
