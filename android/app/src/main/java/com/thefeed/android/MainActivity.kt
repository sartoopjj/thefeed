package com.thefeed.android

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.net.Uri
import android.provider.Settings
import android.text.InputType
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.view.View
import android.view.inputmethod.EditorInfo
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.TextView
import android.webkit.JsResult
import android.webkit.WebChromeClient
import android.webkit.ValueCallback
import android.app.AlertDialog
import androidx.activity.ComponentActivity
import androidx.activity.OnBackPressedCallback
import androidx.activity.result.contract.ActivityResultContracts
import androidx.core.content.ContextCompat
import androidx.core.view.ViewCompat
import androidx.core.view.WindowCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.WindowInsetsControllerCompat
import java.net.HttpURLConnection
import java.net.URL

class MainActivity : ComponentActivity() {
    private lateinit var webView: WebView
    private lateinit var txtStatus: TextView
    private val handler = Handler(Looper.getMainLooper())
    private var fileChooserCallback: ValueCallback<Array<Uri>>? = null
    private var lockScreenVisible = false

    private val fileChooserLauncher = registerForActivityResult(
        ActivityResultContracts.GetContent()
    ) { uri: Uri? ->
        fileChooserCallback?.onReceiveValue(if (uri != null) arrayOf(uri) else emptyArray())
        fileChooserCallback = null
    }

    private val notificationPermissionLauncher = registerForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { /* granted or not, service still works */ }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Let the app draw behind the system status bar
        WindowCompat.setDecorFitsSystemWindows(window, false)
        // Force light (white) status bar icons on dark background
        val controller = WindowInsetsControllerCompat(window, window.decorView)
        controller.isAppearanceLightStatusBars = false
        controller.isAppearanceLightNavigationBars = false
        setContentView(R.layout.activity_main)

        // Apply insets so content isn't hidden behind the status bar or keyboard
        val rootView = findViewById<View>(android.R.id.content)
        ViewCompat.setOnApplyWindowInsetsListener(rootView) { v, insets ->
            val systemBars = insets.getInsets(WindowInsetsCompat.Type.systemBars())
            val ime = insets.getInsets(WindowInsetsCompat.Type.ime())
            v.setPadding(0, systemBars.top, 0, maxOf(systemBars.bottom, ime.bottom))
            insets
        }
        // Trigger inset dispatch explicitly — required on some older Android versions
        ViewCompat.requestApplyInsets(rootView)

        webView = findViewById(R.id.webView)
        txtStatus = findViewById(R.id.txtStatus)

        requestNotificationPermission()
        requestDisableBatteryOptimization()
        configureWebView()
        registerBackHandler()
        startThefeedService()

        if (isPasswordSet()) {
            showLockScreen()
        } else {
            waitForServerThenLoad()
        }
    }

    private fun isPasswordSet(): Boolean {
        val prefs = getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
        return prefs.getString(AndroidBridge.PREF_PASSWORD_HASH, null) != null
    }

    @SuppressLint("SetTextI18n")
    private fun showLockScreen() {
        lockScreenVisible = true
        val lockOverlay = findViewById<LinearLayout>(R.id.lockOverlay)
        val lockTitle = findViewById<TextView>(R.id.lockTitle)
        val lockSubtitle = findViewById<TextView>(R.id.lockSubtitle)
        val lockInput = findViewById<EditText>(R.id.lockPasswordInput)
        val lockBtn = findViewById<Button>(R.id.lockUnlockBtn)
        val lockError = findViewById<TextView>(R.id.lockError)

        val prefs = getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
        val lang = prefs.getString(AndroidBridge.PREF_LANG, "fa") ?: "fa"
        val isPersian = lang == "fa"

        lockTitle.text = getString(R.string.app_name)
        lockSubtitle.text = if (isPersian) "رمز عبور را وارد کنید" else "Enter password to unlock"
        lockInput.hint = if (isPersian) "رمز عبور" else "Password"
        lockBtn.text = if (isPersian) "ورود" else "Unlock"
        if (isPersian) {
            lockOverlay.layoutDirection = View.LAYOUT_DIRECTION_RTL
        }

        lockOverlay.visibility = View.VISIBLE
        webView.visibility = View.GONE
        txtStatus.visibility = View.GONE

        val bridge = AndroidBridge(this)
        val wrongPwText = if (isPersian) "رمز عبور اشتباه است" else "Wrong password"

        fun tryUnlock() {
            val pw = lockInput.text.toString()
            if (bridge.checkPassword(pw)) {
                lockOverlay.visibility = View.GONE
                webView.visibility = View.VISIBLE
                lockScreenVisible = false
                lockInput.text.clear()
                lockError.visibility = View.GONE
                waitForServerThenLoad()
            } else {
                lockError.text = wrongPwText
                lockError.visibility = View.VISIBLE
            }
        }

        lockBtn.setOnClickListener { tryUnlock() }
        lockInput.setOnEditorActionListener { _, actionId, _ ->
            if (actionId == EditorInfo.IME_ACTION_DONE) {
                tryUnlock()
                true
            } else false
        }
    }

    private fun registerBackHandler() {
        onBackPressedDispatcher.addCallback(this, object : OnBackPressedCallback(true) {
            override fun handleOnBackPressed() {
                // Delegate to JS — it knows about open lightboxes, modals,
                // and chat-open state, and can show the close-confirmation
                // dialog at app root. JS calls back through AndroidBridge
                // (minimizeApp / killApp) when the user picks an option.
                webView.evaluateJavascript("window.handleAndroidBack && window.handleAndroidBack();", null)
            }
        })
    }

    private fun requestNotificationPermission() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            if (ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED
            ) {
                notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
        }
    }

    private var batteryOptRequested = false

    @Suppress("BatteryLife")
    private fun requestDisableBatteryOptimization() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
            if (pm.isIgnoringBatteryOptimizations(packageName)) return
            val prefs = getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
            if (prefs.getBoolean(PREF_BATTERY_OPT_DECLINED, false)) return
            batteryOptRequested = true
            val intent = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS).apply {
                data = Uri.parse("package:$packageName")
            }
            try {
                startActivity(intent)
            } catch (_: Exception) {
                batteryOptRequested = false
            }
        }
    }

    override fun onResume() {
        super.onResume()
        if (batteryOptRequested) {
            batteryOptRequested = false
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
                val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
                if (!pm.isIgnoringBatteryOptimizations(packageName)) {
                    // User declined — save preference so we don't ask again
                    getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
                        .edit().putBoolean(PREF_BATTERY_OPT_DECLINED, true).apply()
                }
            }
        }
    }

    private fun startThefeedService() {
        val intent = Intent(this, ThefeedService::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(intent)
        } else {
            startService(intent)
        }
    }

    private fun setStatus(msg: String) {
        txtStatus.text = msg
        txtStatus.visibility = if (msg.isEmpty()) View.GONE else View.VISIBLE
    }

    @SuppressLint("SetJavaScriptEnabled")
    private fun configureWebView() {
        webView.webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(
                view: WebView?,
                request: WebResourceRequest?
            ): Boolean {
                val url = request?.url ?: return false
                // External links (anything not our local server) open in the system browser
                if (url.host != "127.0.0.1") {
                    startActivity(Intent(Intent.ACTION_VIEW, url))
                    return true
                }
                return false
            }

            override fun onPageFinished(view: WebView?, url: String?) {
                if (url != null && url.startsWith("http://127.0.0.1")) {
                    setStatus("")
                }
            }

            override fun onReceivedError(
                view: WebView?,
                request: WebResourceRequest?,
                error: WebResourceError?
            ) {
                // Server was reachable during probe but dropped connection — retry probe cycle
                if (request?.isForMainFrame == true) {
                    setStatus("Reconnecting...")
                    handler.postDelayed({ waitForServerThenLoad() }, RETRY_DELAY_MS)
                }
            }
        }

        // Required for confirm() / alert() / prompt() to work in WebView
        webView.webChromeClient = object : WebChromeClient() {
            override fun onShowFileChooser(
                webView: WebView?,
                filePathCallback: ValueCallback<Array<Uri>>?,
                fileChooserParams: FileChooserParams?
            ): Boolean {
                fileChooserCallback?.onReceiveValue(emptyArray())
                fileChooserCallback = filePathCallback
                val accept = fileChooserParams?.acceptTypes?.firstOrNull() ?: "image/*"
                fileChooserLauncher.launch(accept)
                return true
            }

            override fun onJsConfirm(
                view: WebView?, url: String?, message: String?, result: JsResult?
            ): Boolean {
                AlertDialog.Builder(this@MainActivity)
                    .setMessage(message)
                    .setPositiveButton(android.R.string.ok) { _, _ -> result?.confirm() }
                    .setNegativeButton(android.R.string.cancel) { _, _ -> result?.cancel() }
                    .setOnCancelListener { result?.cancel() }
                    .show()
                return true
            }
        }

        with(webView.settings) {
            javaScriptEnabled = true
            domStorageEnabled = true
            cacheMode = WebSettings.LOAD_DEFAULT
            allowFileAccess = false
            allowContentAccess = false
            mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
        }

        webView.addJavascriptInterface(AndroidBridge(this), "Android")
    }

    /**
     * Polls SharedPreferences for the port on every attempt, then probes the URL.
     * This handles force-kill restarts where the service picks a new port:
     * the loop follows the port change automatically instead of hammering a stale one.
     */
    private fun waitForServerThenLoad() {
        setStatus("Waiting for service...")
        Thread {
            var ready = false
            var lastPort = -1
            for (attempt in 1..MAX_PROBE_ATTEMPTS) {
                val port = getCurrentPort()
                if (port <= 0) {
                    handler.post { setStatus("Waiting for service... ($attempt/$MAX_PROBE_ATTEMPTS)") }
                    Thread.sleep(PROBE_INTERVAL_MS)
                    continue
                }
                if (port != lastPort) {
                    // Service restarted with a new port — reset and start fresh
                    lastPort = port
                    handler.post { setStatus("Connecting...") }
                }
                try {
                    val conn = URL("http://127.0.0.1:$port").openConnection() as HttpURLConnection
                    conn.connectTimeout = PROBE_TIMEOUT_MS.toInt()
                    conn.readTimeout = PROBE_TIMEOUT_MS.toInt()
                    conn.requestMethod = "GET"
                    val code = conn.responseCode
                    conn.disconnect()
                    if (code > 0) {
                        ready = true
                        val url = "http://127.0.0.1:$port"
                        handler.post { setStatus(""); webView.loadUrl(url) }
                        return@Thread
                    }
                } catch (_: Exception) {
                    // Connection refused — not ready yet
                }
                handler.post { setStatus("Waiting for server... ($attempt/$MAX_PROBE_ATTEMPTS)") }
                Thread.sleep(PROBE_INTERVAL_MS)
            }
            if (!ready) {
                handler.post { setStatus("Could not connect. Restart the app.") }
            }
        }.start()
    }

    private fun getCurrentPort(): Int {
        val prefs = getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
        return prefs.getInt(ThefeedService.PREF_PORT, -1)
    }

    override fun onDestroy() {
        handler.removeCallbacksAndMessages(null)
        webView.destroy()
        super.onDestroy()
    }

    companion object {
        private const val MAX_PROBE_ATTEMPTS = 30
        private const val PROBE_INTERVAL_MS = 1000L   // 1s between probes → up to 30s total
        private const val PROBE_TIMEOUT_MS  = 1000L   // 1s HTTP connect timeout per probe
        private const val RETRY_DELAY_MS    = 2000L   // delay before restarting probe cycle on error
        private const val PREF_BATTERY_OPT_DECLINED = "battery_opt_declined"
    }
}

