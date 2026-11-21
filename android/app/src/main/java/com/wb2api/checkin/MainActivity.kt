package com.wb2api.checkin

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.ui.platform.LocalContext
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import com.wb2api.checkin.data.readImportFiles
import com.wb2api.checkin.ui.AppScreen
import com.wb2api.checkin.ui.WbCheckinTheme
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContent {
            WbCheckinTheme {
                val context = LocalContext.current
                val factory = remember { CheckinViewModel.Factory(context) }
                val viewModel: CheckinViewModel = viewModel(factory = factory)
                val state by viewModel.state.collectAsStateWithLifecycle()
                val scope = rememberCoroutineScope()
                val launcher = rememberLauncherForActivityResult(
                    contract = ActivityResultContracts.OpenMultipleDocuments(),
                ) { uris ->
                    if (uris.isNotEmpty()) {
                        scope.launch {
                            val files = withContext(Dispatchers.IO) {
                                readImportFiles(context.contentResolver, uris)
                            }
                            viewModel.importFiles(files)
                        }
                    }
                }
                AppScreen(
                    state = state,
                    onImport = {
                        launcher.launch(
                            arrayOf(
                                "application/json",
                                "text/plain",
                                "application/octet-stream",
                            ),
                        )
                    },
                    onCheckin = viewModel::checkin,
                    onCheckinAll = viewModel::checkinAll,
                    onDismissImport = viewModel::clearImportSummary,
                    onDismissError = viewModel::clearError,
                )
            }
        }
    }
}
