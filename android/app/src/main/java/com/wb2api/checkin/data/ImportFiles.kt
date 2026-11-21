package com.wb2api.checkin.data

import android.content.ContentResolver
import android.net.Uri
import android.provider.OpenableColumns

fun readImportFiles(resolver: ContentResolver, uris: List<Uri>): List<ImportFile> =
    uris.map { uri ->
        val name = queryName(resolver, uri)
        val content = runCatching {
            resolver.openInputStream(uri)?.use { it.readBytes().toString(Charsets.UTF_8) }
                ?: error("无法读取文件")
        }.getOrNull()
        ImportFile(name, content)
    }

private fun queryName(resolver: ContentResolver, uri: Uri): String {
    runCatching {
        resolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { c ->
            if (c.moveToFirst()) {
                val idx = c.getColumnIndex(OpenableColumns.DISPLAY_NAME)
                if (idx >= 0) c.getString(idx)?.let { return it }
            }
        }
    }
    return uri.lastPathSegment ?: "account.json"
}
