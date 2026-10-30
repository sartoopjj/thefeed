package com.thefeed.android

import android.Manifest
import android.app.Activity
import android.content.ContentValues
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Color
import android.net.Uri
import android.os.Build
import android.os.Environment
import android.os.Handler
import android.os.Looper
import android.provider.MediaStore
import android.util.Base64
import android.util.Log
import android.view.View
import android.webkit.JavascriptInterface
import android.widget.Toast
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat
import androidx.core.content.FileProvider
import androidx.core.view.WindowInsetsControllerCompat
import java.io.File
import java.io.FileOutputStream
import java.security.MessageDigest

class AndroidBridge(private val activity: Activity) {

    private val prefs by lazy {
        activity.getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
    }

    @JavascriptInterface
    fun isAndroid(): Boolean = true

    // Recolor the status bar + gesture/navigation bar to match the web theme so
    // they don't stay black against a light app. colorHex is the resolved
    // --bg2; dark=true means the app is in dark theme (→ light bar icons).
    @JavascriptInterface
    fun setSystemBars(colorHex: String, dark: Boolean) {
        activity.runOnUiThread {
            val color = try {
                Color.parseColor(colorHex.trim())
            } catch (e: Exception) {
                Color.parseColor(if (dark) "#0e1621" else "#f0f2f5")
            }
            val window = activity.window
            @Suppress("DEPRECATION")
            window.statusBarColor = color
            @Suppress("DEPRECATION")
            window.navigationBarColor = color
            // Fill the inset gaps behind the bars too (covers Android 15's
            // enforced edge-to-edge, where the *BarColor setters are ignored).
            activity.findViewById<View>(android.R.id.content)?.setBackgroundColor(color)
            val controller = WindowInsetsControllerCompat(window, window.decorView)
            // Light bars (dark icons) in light theme; light icons in dark theme.
            controller.isAppearanceLightStatusBars = !dark
            controller.isAppearanceLightNavigationBars = !dark
        }
    }

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
        // Empty string when never set — the first-run language modal
        // uses this to decide whether to show. A default like "fa"
        // would silently suppress the modal on a fresh install.
        return prefs.getString(PREF_LANG, "") ?: ""
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
        // The hash is only the lock screen. This is what makes the passcode
        // worth something at rest: the Telegram session and message cache are
        // sealed under a data key, and in device mode that key is a file
        // beside the data. Wrapping it under the passcode means the files
        // alone are not enough. The SAME key is re-wrapped, never replaced,
        // so an existing session is not orphaned.
        telLock(password)
        return true
    }

    @JavascriptInterface
    fun removePassword(currentPassword: String): Boolean {
        val stored = prefs.getString(PREF_PASSWORD_HASH, null) ?: return false
        if (sha256(currentPassword) != stored) return false
        prefs.edit().remove(PREF_PASSWORD_HASH).apply()
        // Back to a device-held key, or the data would stay sealed under a
        // passcode the user has just removed and can no longer be asked for.
        telPost("/api/tel/lock/remove", null)
        return true
    }

    @JavascriptInterface
    fun checkPassword(password: String): Boolean {
        val stored = prefs.getString(PREF_PASSWORD_HASH, null) ?: return true
        return sha256(password) == stored
    }

    // ===== Binding the passcode to the Telegram data key =====

    // telLock wraps the data key under the passcode. Runs off the caller's
    // thread: Argon2id is deliberately slow, and setPassword is called from
    // the WebView's JS thread.
    fun telLock(password: String) {
        Thread { telPost("/api/tel/lock", password) }.start()
    }

    // telUnlock opens the sealed store for this run. Synchronous on purpose:
    // the lock screen calls it as the user is let in, and the Telegram
    // section must not start reading a store whose key has not arrived yet.
    fun telUnlock(password: String): Boolean = telPost("/api/tel/unlock", password)

    // telPost calls the local API with the per-launch Basic secret. Returns
    // false on anything other than success -- callers treat that as "not
    // bound", which is the pre-existing behaviour, never as a reason to block
    // the user.
    private fun telPost(path: String, password: String?): Boolean {
        val port = prefs.getInt(ThefeedService.PREF_PORT, -1)
        val secret = prefs.getString(ThefeedService.PREF_LOCAL_SECRET, "") ?: ""
        if (port <= 0 || secret.isEmpty()) return false
        return try {
            val conn = (java.net.URL("http://127.0.0.1:$port$path").openConnection()
                    as java.net.HttpURLConnection).apply {
                requestMethod = "POST"
                connectTimeout = 5_000
                // Argon2id at 64 MiB takes a moment on a weak phone.
                readTimeout = 30_000
                doOutput = true
                setRequestProperty("Content-Type", "application/json")
                // Any user name is accepted; only the password is checked.
                val basic = android.util.Base64.encodeToString(
                    "thefeed:$secret".toByteArray(), android.util.Base64.NO_WRAP)
                setRequestProperty("Authorization", "Basic $basic")
            }
            conn.outputStream.use {
                val body = if (password == null) "{}"
                else org.json.JSONObject().put("passphrase", password).toString()
                it.write(body.toByteArray())
            }
            val ok = conn.responseCode / 100 == 2
            conn.disconnect()
            ok
        } catch (_: Exception) {
            false
        }
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

    // ── Streaming download (app updates) ─────────────────────────────────
    // The web UI used to pull the whole APK into JS memory and hand it over
    // as base64. That peaked around 8x the file size — roughly 210 MB for the
    // 25 MB universal APK, counting the JS chunks, the Blob, the UTF-16
    // base64 string, its copy across the JNI boundary and the decoded bytes —
    // which is enough to push a low-RAM device into system-wide thrashing.
    // Streaming straight to Downloads here keeps memory flat at one buffer.
    // JS starts the transfer and then polls downloadStatus().

    private val dlLock = Any()
    private var dlState = "idle" // idle | running | done | error | canceled
    private var dlReceived = 0L
    private var dlTotal = 0L
    private var dlDetail = "" // saved name on success, message on error

    // Read by the transfer thread on every buffer, so they must not be cached.
    @Volatile private var dlCancel = false
    // Bumped per download. A worker whose generation is stale must not write
    // state or keep writing bytes — otherwise a cancel-then-retry lets the
    // abandoned transfer finish and clobber the new one's status.
    @Volatile private var dlGen = 0

    /** Raised on the transfer thread when the user cancels. */
    private class DownloadCanceled : java.io.IOException("canceled")

    /** Starts a background download of url into Downloads/. Returns false if
     *  it could not be started (storage permission still pending). */
    @JavascriptInterface
    fun startDownload(url: String, filename: String): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q && !hasLegacyStoragePermission()) {
            requestLegacyStoragePermission()
            toast("Storage permission needed — please tap Update again")
            return false
        }
        var gen = 0
        synchronized(dlLock) {
            if (dlState == "running") return true // already in flight
            dlCancel = false
            dlGen++
            gen = dlGen
            dlState = "running"
            dlReceived = 0L
            dlTotal = 0L
            dlDetail = ""
        }
        val safe = sanitiseFilename(filename)
        Thread({ runDownload(url, safe, gen) }, "thefeed-download").apply {
            isDaemon = true
            start()
        }
        return true
    }

    /** Aborts an in-flight download; the partial file is discarded. Without
     *  this the transfer ran to completion after the user cancelled. */
    @JavascriptInterface
    fun cancelDownload() {
        synchronized(dlLock) {
            if (dlState != "running") return
            // Release the slot now. The worker may be parked in read() until
            // the 60s timeout; without this a retry would be refused as
            // "already in flight" and silently start nothing.
            dlState = "canceled"
            dlDetail = ""
        }
        dlCancel = true
    }

    /** Progress for the JS poller: {"state","received","total","detail"}. */
    @JavascriptInterface
    fun downloadStatus(): String = synchronized(dlLock) {
        "{\"state\":\"$dlState\",\"received\":$dlReceived," +
            "\"total\":$dlTotal,\"detail\":\"${jsonEscape(dlDetail)}\"}"
    }

    /** Ignores writes from a superseded worker. */
    private fun finishDownload(gen: Int, state: String, detail: String) {
        synchronized(dlLock) {
            if (gen != dlGen) return
            dlState = state
            dlDetail = detail
        }
    }

    /** Drops a half-written file so Downloads never holds a truncated APK. */
    private fun discardPartial(resolver: android.content.ContentResolver, target: Uri?, legacy: File?) {
        if (target != null) {
            try { resolver.delete(target, null, null) } catch (ignored: Exception) { }
        }
        if (legacy != null) {
            try { legacy.delete() } catch (ignored: Exception) { }
        }
    }

    private fun runDownload(url: String, fallbackName: String, gen: Int) {
        val resolver = activity.contentResolver
        var target: Uri? = null
        var legacy: File? = null // pre-Q has no pending flag; delete by path
        var safe = fallbackName
        try {
            val conn = (java.net.URL(url).openConnection() as java.net.HttpURLConnection).apply {
                connectTimeout = 30_000
                readTimeout = 60_000
                instanceFollowRedirects = true
            }
            try {
                if (conn.responseCode / 100 != 2) throw java.io.IOException("HTTP ${conn.responseCode}")
                val len = conn.contentLengthLong
                synchronized(dlLock) { if (gen == dlGen) dlTotal = if (len > 0) len else 0L }
                // The server names the asset (it encodes the ABI, e.g.
                // ...-arm64-v8a.apk); fall back to the caller's guess.
                val header = conn.getHeaderField("X-Download-Filename")
                if (!header.isNullOrBlank()) safe = sanitiseFilename(header)

                val out: java.io.OutputStream
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                    val values = ContentValues().apply {
                        put(MediaStore.MediaColumns.DISPLAY_NAME, safe)
                        put(MediaStore.MediaColumns.MIME_TYPE, mimeFromFilename(safe, "application/octet-stream"))
                        put(MediaStore.MediaColumns.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS)
                        put(MediaStore.MediaColumns.IS_PENDING, 1)
                    }
                    target = resolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values)
                        ?: throw java.io.IOException("no Downloads URI")
                    out = resolver.openOutputStream(target)
                        ?: throw java.io.IOException("cannot open Downloads")
                } else {
                    @Suppress("DEPRECATION")
                    val dir = Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS)
                    if (!dir.exists()) dir.mkdirs()
                    val f = File(dir, safe)
                    legacy = f
                    out = FileOutputStream(f)
                }

                conn.inputStream.use { input ->
                    out.use { os ->
                        val buf = ByteArray(64 * 1024)
                        var got = 0L
                        while (true) {
                            if (dlCancel || gen != dlGen) throw DownloadCanceled()
                            val n = input.read(buf)
                            if (n < 0) break
                            os.write(buf, 0, n)
                            got += n
                            synchronized(dlLock) { if (gen == dlGen) dlReceived = got }
                        }
                        os.flush()
                    }
                }
            } finally {
                conn.disconnect()
            }

            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q && target != null) {
                resolver.update(
                    target,
                    ContentValues().apply { put(MediaStore.MediaColumns.IS_PENDING, 0) },
                    null, null
                )
            }
            finishDownload(gen, "done", safe)
            toast("Saved to Downloads/$safe")
        } catch (e: DownloadCanceled) {
            discardPartial(resolver, target, legacy)
            finishDownload(gen, "canceled", "")
        } catch (e: Exception) {
            Log.e(TAG, "startDownload failed", e)
            discardPartial(resolver, target, legacy)
            finishDownload(gen, "error", e.message ?: "download failed")
        }
    }

    private fun jsonEscape(s: String): String {
        val sb = StringBuilder(s.length + 8)
        for (c in s) {
            when {
                c == '"' -> sb.append("\\\"")
                c == '\\' -> sb.append("\\\\")
                c == '\n' -> sb.append("\\n")
                c == '\r' -> sb.append("\\r")
                c == '\t' -> sb.append("\\t")
                c < ' ' -> sb.append(String.format("\\u%04x", c.code))
                else -> sb.append(c)
            }
        }
        return sb.toString()
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
