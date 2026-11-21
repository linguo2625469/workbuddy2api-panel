package com.wb2api.checkin.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import com.wb2api.checkin.AccountRow
import com.wb2api.checkin.UiState
import com.wb2api.checkin.data.ImportSummary

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AppScreen(
    state: UiState,
    onImport: () -> Unit,
    onCheckin: (String) -> Unit,
    onCheckinAll: () -> Unit,
    onDismissImport: () -> Unit,
    onDismissError: () -> Unit,
) {
    val snackbar = remember { SnackbarHostState() }
    LaunchedEffect(state.error) {
        state.error?.let {
            snackbar.showSnackbar(it)
            onDismissError()
        }
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("WB 签到") },
                actions = {
                    TextButton(onClick = onImport, enabled = !state.busy) { Text("导入") }
                },
            )
        },
        snackbarHost = { SnackbarHost(snackbar) },
    ) { padding ->
        Column(
            modifier = Modifier
                .padding(padding)
                .fillMaxSize()
                .padding(horizontal = 16.dp),
        ) {
            Spacer(Modifier.height(8.dp))
            Button(
                onClick = onCheckinAll,
                enabled = !state.busy && state.rows.isNotEmpty(),
                modifier = Modifier.fillMaxWidth(),
            ) {
                val label = if (state.busy && state.progressTotal > 0) {
                    "签到中 ${state.progressDone}/${state.progressTotal}"
                } else {
                    "全部签到"
                }
                Text(label)
            }
            Spacer(Modifier.height(12.dp))
            when {
                state.loading -> Box(
                    modifier = Modifier
                        .weight(1f)
                        .fillMaxWidth(),
                    contentAlignment = Alignment.Center,
                ) {
                    CircularProgressIndicator()
                }

                state.rows.isEmpty() -> Box(
                    modifier = Modifier
                        .weight(1f)
                        .fillMaxWidth(),
                ) {
                    EmptyState(onImport, enabled = !state.busy)
                }

                else -> LazyColumn(
                    modifier = Modifier
                        .weight(1f)
                        .fillMaxWidth(),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    items(state.rows, key = { it.account.uid }) { row ->
                        AccountCard(
                            row = row,
                            enabled = !state.busy,
                            onCheckin = { onCheckin(row.account.uid) },
                        )
                    }
                }
            }
        }
    }

    state.importSummary?.let { summary ->
        ImportDialog(summary = summary, onDismiss = onDismissImport)
    }
}

@Composable
private fun AccountCard(row: AccountRow, enabled: Boolean, onCheckin: () -> Unit) {
    Card(modifier = Modifier.fillMaxWidth()) {
        Row(
            modifier = Modifier.padding(12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f)) {
                Text(
                    text = row.account.nickname.ifBlank { row.account.uid.take(8) },
                    style = MaterialTheme.typography.titleMedium,
                )
                Spacer(Modifier.height(2.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                    Text(
                        text = row.account.uid.take(8),
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    Tag(if (row.account.realm == "global") "国际" else "国内")
                    if (row.account.enterprise) Tag("企业")
                }
                row.result?.let { result ->
                    Spacer(Modifier.height(6.dp))
                    Text(
                        text = result.message,
                        style = MaterialTheme.typography.bodySmall,
                        color = statusColor(result.status),
                    )
                }
            }
            Column(horizontalAlignment = Alignment.End) {
                StatusChip(row.result?.status)
                Spacer(Modifier.height(6.dp))
                OutlinedButton(onClick = onCheckin, enabled = enabled) { Text("签到") }
            }
        }
    }
}

@Composable
private fun StatusChip(status: String?) {
    val label = when (status) {
        null -> "未签到"
        "ok" -> "签到成功"
        "already" -> "今天已签到"
        "skipped" -> "已跳过"
        "auth_invalid" -> "凭证失效"
        "failed" -> "失败"
        else -> "未知"
    }
    val color = when (status) {
        null -> MaterialTheme.colorScheme.onSurfaceVariant
        "ok" -> Color(0xFF2E7D32)
        "already" -> Color(0xFF1565C0)
        "skipped" -> Color(0xFFF9A825)
        else -> MaterialTheme.colorScheme.error
    }
    Box(
        modifier = Modifier
            .background(color.copy(alpha = 0.15f), RoundedCornerShape(12.dp))
            .padding(horizontal = 8.dp, vertical = 4.dp),
    ) {
        Text(label, style = MaterialTheme.typography.labelSmall, color = color)
    }
}

@Composable
private fun Tag(text: String) {
    Box(
        modifier = Modifier
            .background(MaterialTheme.colorScheme.secondaryContainer, RoundedCornerShape(4.dp))
            .padding(horizontal = 6.dp, vertical = 2.dp),
    ) {
        Text(
            text = text,
            style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.onSecondaryContainer,
        )
    }
}

@Composable
private fun statusColor(status: String): Color = when (status) {
    "ok" -> Color(0xFF2E7D32)
    "already" -> Color(0xFF1565C0)
    "skipped" -> Color(0xFFF9A825)
    "auth_invalid", "failed" -> MaterialTheme.colorScheme.error
    else -> MaterialTheme.colorScheme.onSurfaceVariant
}

@Composable
private fun EmptyState(onImport: () -> Unit, enabled: Boolean) {
    Column(
        modifier = Modifier.fillMaxSize(),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.Center,
    ) {
        Text("还没有账号", style = MaterialTheme.typography.titleMedium)
        Spacer(Modifier.height(8.dp))
        Text(
            "把面板 auths/ 里的 workbuddy-*.json 导入手机",
            style = MaterialTheme.typography.bodyMedium,
            textAlign = TextAlign.Center,
        )
        Spacer(Modifier.height(16.dp))
        Button(onClick = onImport, enabled = enabled) { Text("导入账号") }
    }
}

@Composable
private fun ImportDialog(summary: ImportSummary, onDismiss: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        confirmButton = { TextButton(onClick = onDismiss) { Text("知道了") } },
        title = { Text("导入结果") },
        text = {
            Column(modifier = Modifier.verticalScroll(rememberScrollState())) {
                Text("成功 ${summary.ok} / 共 ${summary.total} 个文件")
                if (summary.failures.isNotEmpty()) {
                    Spacer(Modifier.height(8.dp))
                    summary.failures.forEach {
                        Text("• $it", style = MaterialTheme.typography.bodySmall)
                    }
                }
            }
        },
    )
}
