package com.thefeed.android

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.IBinder
import androidx.core.app.NotificationCompat
import java.io.File
import java.net.ServerSocket

class ThefeedService : Service() {
    private var process: Process? = null
    private var readerThread: Thread? = null
    private var currentPort: Int = -1

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
        startForeground(NOTIFICATION_ID, buildNotification("Starting local service..."))
        savePort(-1)  // Clear stale port from any previous (force-killed) session
        startClientProcessAsync()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopSelf()
            return START_NOT_STICKY
        }
        // If the process died, restart it
        if (process == null || !isProcessAlive()) {
            startClientProcessAsync()
        }
        return START_STICKY
    }

    override fun onDestroy() {
        readerThread?.interrupt()
        readerThread = null
        process?.destroy()
        try {
            process?.waitFor()
        } catch (_: Exception) {
        }
        process = null
        savePort(-1)
        super.onDestroy()
        // Kill the entire app process so the activity doesn't remain open
        android.os.Process.killProcess(android.os.Process.myPid())
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun isProcessAlive(): Boolean {
        return try {
            process?.exitValue()
            false // exitValue() returned, so the process has exited
        } catch (_: IllegalThreadStateException) {
            true // still running
        }
    }

    private fun startClientProcessAsync() {
        // Don't spawn a second process
        if (process != null && isProcessAlive()) return

        Thread {
            try {
                val bin = nativeBin()
                val dataDir = File(filesDir, "thefeeddata")
                if (!dataDir.exists()) dataDir.mkdirs()

                // Reuse the last port so the WebView origin stays
                // stable — keeps localStorage state across launches.
                val selectedPort = pickPort()
                currentPort = selectedPort
                savePort(selectedPort)

                val env = mutableMapOf<String, String>()
                env["HOME"] = filesDir.absolutePath
                env["TMPDIR"] = cacheDir.absolutePath
                // Tells internal/update to point the user at the APK on
                // GitHub instead of the bare client binary.
                env["THEFEED_ANDROID_APK"] = "1"

                val pb = ProcessBuilder(
                    bin.absolutePath,
                    "--data-dir", dataDir.absolutePath,
                    "--port", selectedPort.toString()
                )
                pb.directory(dataDir)
                pb.redirectErrorStream(true)
                pb.environment().putAll(env)

                process = pb.start()

                readerThread = Thread {
                    try {
                        process?.inputStream?.bufferedReader()?.use { reader ->
                            while (!Thread.currentThread().isInterrupted) {
                                val line = reader.readLine() ?: break
                                updateForegroundNotification(line)
                            }
                        }
                    } catch (_: Exception) {
                    }
                }
                readerThread?.isDaemon = true
                readerThread?.start()

                updateForegroundNotification("Running on http://127.0.0.1:$selectedPort")
            } catch (e: Exception) {
                savePort(-1)
                updateForegroundNotification("Failed: ${e.message ?: e.javaClass.simpleName}")
            }
        }.start()
    }

    /**
     * The Go binary is packaged as libthefeed.so in jniLibs/ so the package
     * installer places it in nativeLibraryDir — the only directory Android allows
     * execution from (W^X policy blocks exec from filesDir on Android 10+).
     */
    private fun nativeBin(): File {
        val bin = File(applicationInfo.nativeLibraryDir, "libthefeed.so")
        if (!bin.exists()) {
            throw IllegalStateException(
                "Native binary missing — reinstall the app. Expected: ${bin.absolutePath}"
            )
        }
        return bin
    }

    private fun findFreePort(): Int {
        ServerSocket(0).use { socket ->
            socket.reuseAddress = true
            return socket.localPort
        }
    }

    // Try the last port first; fall back to a new free one if it's
    // taken. Keeps localStorage origin stable across launches.
    private fun pickPort(): Int {
        val prefs = getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
        val last = prefs.getInt(PREF_PORT, -1)
        if (last in 1024..65535) {
            try {
                ServerSocket(last).use { it.reuseAddress = true }
                return last
            } catch (_: Exception) { }
        }
        return findFreePort()
    }

    private fun savePort(port: Int) {
        val prefs = getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
        prefs.edit().putInt(PREF_PORT, port).apply()
    }

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                "thefeed background service",
                NotificationManager.IMPORTANCE_LOW
            ).apply {
                description = "Keeps thefeed client running"
                setShowBadge(false)
            }
            val manager = getSystemService(NotificationManager::class.java)
            manager.createNotificationChannel(channel)
        }
    }

    private fun buildNotification(message: String): Notification {
        val openIntent = Intent(this, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_SINGLE_TOP
        }
        val pendingIntent = PendingIntent.getActivity(
            this, 0, openIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

        val stopIntent = Intent(this, ThefeedService::class.java).apply {
            action = ACTION_STOP
        }
        val stopPendingIntent = PendingIntent.getService(
            this, 1, stopIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle("thefeed")
            .setContentText(message)
            .setSmallIcon(android.R.drawable.stat_notify_sync)
            .setOngoing(true)
            .setContentIntent(pendingIntent)
            .addAction(android.R.drawable.ic_menu_close_clear_cancel, "Exit", stopPendingIntent)
            .setSilent(true)
            .build()
    }

    private fun updateForegroundNotification(message: String) {
        try {
            val manager = getSystemService(NotificationManager::class.java)
            manager.notify(NOTIFICATION_ID, buildNotification(message))
        } catch (_: Exception) {
            // Notification permission may not be granted; service still runs
        }
    }

    companion object {
        const val CHANNEL_ID = "thefeed_service"
        const val NOTIFICATION_ID = 1201
        const val PREFS_NAME = "thefeed_runtime"
        const val PREF_PORT = "port"
        const val ACTION_STOP = "com.thefeed.android.STOP"
    }
}
