package com.thefeed.android

import android.Manifest
import android.app.Activity
import android.content.ContentValues
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Environment
import android.os.Handler
import android.os.Looper
import android.provider.MediaStore
import android.util.Base64
import android.util.Log
import android.webkit.JavascriptInterface
import android.widget.Toast
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat
import androidx.core.content.FileProvider
import java.io.File
import java.io.FileOutputStream
import java.security.MessageDigest

class AndroidBridge(private val activity: Activity) {

    private val prefs by lazy {
        activity.getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
    }

    @JavascriptInterface
    fun isAndroid(): Boolean = true

    // ===== App lifecycle (back-button confirmation) =====

    @JavascriptInterface
    fun minimizeApp() {
        Handler(Looper.getMainLooper()).post {
            activity.moveTaskToBack(true)
        }
    }

    @JavascriptInterface
    fun killApp() {
        Handler(Looper.getMainLooper()).post {
            activity.stopService(Intent(activity, ThefeedService::class.java))
            activity.finishAndRemoveTask()
            // Belt-and-braces: ensure the JVM exits even if a stray
            // foreground notification or pending Intent keeps the
            // process alive after finishAndRemoveTask().
            android.os.Process.killProcess(android.os.Process.myPid())
        }
    }

    // ===== Language =====

    @JavascriptInterface
    fun setLang(lang: String) {
        prefs.edit().putString(PREF_LANG, lang).apply()
    }

    @JavascriptInterface
    fun getLang(): String {
        return prefs.getString(PREF_LANG, "fa") ?: "fa"
    }

    // ===== Password =====

    @JavascriptInterface
    fun hasPassword(): Boolean {
        return prefs.getString(PREF_PASSWORD_HASH, null) != null
    }

    @JavascriptInterface
    fun setPassword(password: String): Boolean {
        if (password.isEmpty()) return false
        prefs.edit().putString(PREF_PASSWORD_HASH, sha256(password)).apply()
        return true
    }

    @JavascriptInterface
    fun removePassword(currentPassword: String): Boolean {
        val stored = prefs.getString(PREF_PASSWORD_HASH, null) ?: return false
        if (sha256(currentPassword) != stored) return false
        prefs.edit().remove(PREF_PASSWORD_HASH).apply()
        return true
    }

    @JavascriptInterface
    fun checkPassword(password: String): Boolean {
        val stored = prefs.getString(PREF_PASSWORD_HASH, null) ?: return true
        return sha256(password) == stored
    }

    // ===== Media handoff to system apps =====
    // The web frontend calls these for save / open / share when it
    // detects window.Android — WebView can't natively download blob URLs,
    // navigate to blob URLs in new tabs, or do navigator.share with files.

    @JavascriptInterface
    fun openMedia(base64: String, mime: String, filename: String): Boolean {
        return try {
            val uri = writeToCache(base64, filename)
            val resolvedMime = mimeFromFilename(filename, mime)
            val intent = Intent(Intent.ACTION_VIEW).apply {
                setDataAndType(uri, resolvedMime)
                putExtra(Intent.EXTRA_TITLE, filename)
                addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
            }
            // Try the user's default for this MIME first. Android only
            // shows a chooser if no default is set; that's the right
            // behaviour for "play" — we want the actual video player,
            // not a generic file-handler picker that might copy/download.
            try {
                activity.startActivity(intent)
            } catch (_: Exception) {
                activity.startActivity(Intent.createChooser(intent, filename))
            }
            true
        } catch (e: Exception) {
            Log.e(TAG, "openMedia failed", e)
            toast("Open failed: ${e.message}")
            false
        }
    }

    @JavascriptInterface
    fun shareMedia(base64: String, mime: String, filename: String): Boolean {
        return try {
            val uri = writeToCache(base64, filename)
            val intent = Intent(Intent.ACTION_SEND).apply {
                type = mimeFromFilename(filename, mime)
                putExtra(Intent.EXTRA_STREAM, uri)
                addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
            }
            activity.startActivity(Intent.createChooser(intent, filename))
            true
        } catch (_: Exception) { false }
    }

    @JavascriptInterface
    fun saveMedia(base64: String, mime: String, filename: String): Boolean {
        val bytes = try { Base64.decode(base64, Base64.DEFAULT) }
                   catch (e: Exception) { toast("Save failed: bad data"); Log.e(TAG, "saveMedia decode", e); return false }
        val safe = sanitiseFilename(filename)
        // Android 9 and below need WRITE_EXTERNAL_STORAGE granted at
        // runtime — manifest entry alone isn't enough for "dangerous"
        // permissions. Request it lazily; the system dialog returns
        // asynchronously, so this call bails and the user re-clicks
        // Save after granting.
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) {
            if (!hasLegacyStoragePermission()) {
                requestLegacyStoragePermission()
                toast("Storage permission needed — please tap Save again")
                return false
            }
        }
        return try {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                val resolver = activity.contentResolver
                val collection = MediaStore.Downloads.EXTERNAL_CONTENT_URI
                val values = ContentValues().apply {
                    put(MediaStore.MediaColumns.DISPLAY_NAME, safe)
                    put(MediaStore.MediaColumns.MIME_TYPE, mimeFromFilename(safe, mime))
                    put(MediaStore.MediaColumns.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS)
                    put(MediaStore.MediaColumns.IS_PENDING, 1)
                }
                val target = resolver.insert(collection, values)
                if (target == null) {
                    toast("Save failed: no Downloads URI")
                    return false
                }
                resolver.openOutputStream(target).use { os ->
                    if (os == null) {
                        resolver.delete(target, null, null)
                        toast("Save failed: cannot open Downloads")
                        return false
                    }
                    os.write(bytes)
                    os.flush()
                }
                val done = ContentValues().apply { put(MediaStore.MediaColumns.IS_PENDING, 0) }
                resolver.update(target, done, null, null)
                toast("Saved to Downloads/$safe")
            } else {
                @Suppress("DEPRECATION")
                val dir = Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS)
                if (!dir.exists()) dir.mkdirs()
                val out = File(dir, safe)
                FileOutputStream(out).use { it.write(bytes) }
                toast("Saved to ${out.absolutePath}")
            }
            true
        } catch (e: Exception) {
            Log.e(TAG, "saveMedia failed", e)
            toast("Save failed: ${e.message}")
            false
        }
    }

    private fun hasLegacyStoragePermission(): Boolean {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) return true
        return ContextCompat.checkSelfPermission(
            activity,
            Manifest.permission.WRITE_EXTERNAL_STORAGE
        ) == PackageManager.PERMISSION_GRANTED
    }

    private fun requestLegacyStoragePermission() {
        Handler(Looper.getMainLooper()).post {
            ActivityCompat.requestPermissions(
                activity,
                arrayOf(Manifest.permission.WRITE_EXTERNAL_STORAGE),
                REQ_LEGACY_STORAGE
            )
        }
    }

    private fun toast(msg: String) {
        Handler(Looper.getMainLooper()).post {
            Toast.makeText(activity, msg, Toast.LENGTH_LONG).show()
        }
    }

    private fun writeToCache(base64: String, filename: String): Uri {
        val bytes = Base64.decode(base64, Base64.DEFAULT)
        val dir = File(activity.cacheDir, "shared")
        dir.mkdirs()
        val out = File(dir, sanitiseFilename(filename))
        FileOutputStream(out).use { it.write(bytes) }
        return FileProvider.getUriForFile(activity, activity.packageName + ".fileprovider", out)
    }

    private fun sanitiseFilename(name: String): String {
        val cleaned = name.replace(Regex("[^A-Za-z0-9._-]"), "_").trim('_', '.')
        return if (cleaned.isEmpty()) "media" else cleaned
    }

    private fun sanitiseMime(m: String): String {
        return if (m.matches(Regex("^[\\w./+-]+$"))) m else "application/octet-stream"
    }

    // Derive a MIME type from the filename's extension. When the bytes
    // were sniffed by Go's http.DetectContentType (e.g., APK files come
    // back as application/zip because APKs ARE zips), passing that MIME
    // to MediaStore would make Android append ".zip" to the filename.
    // Trusting the extension fixes the *.apk.zip / *.docx.zip surprise.
    private fun mimeFromFilename(name: String, fallback: String): String {
        val dot = name.lastIndexOf('.')
        if (dot < 0 || dot == name.length - 1) return sanitiseMime(fallback)
        val ext = name.substring(dot + 1).lowercase()
        return when (ext) {
            "apk" -> "application/vnd.android.package-archive"
            "pdf" -> "application/pdf"
            "zip" -> "application/zip"
            "mp3" -> "audio/mpeg"
            "ogg", "oga", "opus" -> "audio/ogg"
            "wav" -> "audio/wav"
            "m4a" -> "audio/mp4"
            "mp4", "m4v" -> "video/mp4"
            "webm" -> "video/webm"
            "mkv" -> "video/x-matroska"
            "mov" -> "video/quicktime"
            "jpg", "jpeg" -> "image/jpeg"
            "png" -> "image/png"
            "gif" -> "image/gif"
            "webp" -> "image/webp"
            "svg" -> "image/svg+xml"
            "txt" -> "text/plain"
            "html", "htm" -> "text/html"
            "json" -> "application/json"
            "doc" -> "application/msword"
            "docx" -> "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
            "xls" -> "application/vnd.ms-excel"
            "xlsx" -> "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
            "ppt" -> "application/vnd.ms-powerpoint"
            "pptx" -> "application/vnd.openxmlformats-officedocument.presentationml.presentation"
            // Unknown extension: don't trust the sniffed fallback (often
            // text/plain, which makes MediaStore append .txt). Generic
            // binary leaves the filename verbatim.
            else -> "application/octet-stream"
        }
    }

    private fun sha256(input: String): String {
        val digest = MessageDigest.getInstance("SHA-256")
        val hash = digest.digest(input.toByteArray(Charsets.UTF_8))
        return hash.joinToString("") { "%02x".format(it) }
    }

    companion object {
        const val PREF_PASSWORD_HASH = "password_hash"
        const val PREF_LANG = "app_lang"
        const val REQ_LEGACY_STORAGE = 0x501
        private const val TAG = "AndroidBridge"
    }
}
