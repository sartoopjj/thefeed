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
import android.webkit.HttpAuthHandler
import android.webkit.RenderProcessGoneDetail
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
import androidx.core.app.NotificationManagerCompat
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

    // OpenDocument shows the full system file manager (not just the Photo Picker)
    // so users can pick any file type, not just images/video.
    private val fileChooserLauncher = registerForActivityResult(
        ActivityResultContracts.OpenDocument()
    ) { uri: Uri? ->
        fileChooserCallback?.onReceiveValue(if (uri != null) arrayOf(uri) else emptyArray())
        fileChooserCallback = null
    }

    // HTML5 video fullscreen support: WebView calls onShowCustomView when
    // the page enters fullscreen and onHideCustomView when it exits. Without
    // these the native player's fullscreen button stays greyed out.
    private var fullscreenView: View? = null
    private var fullscreenCallback: WebChromeClient.CustomViewCallback? = null

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
                // Held only until the probe below can reach the server, which
                // may still be starting: the Telegram data key is wrapped
                // under this passcode and has to be opened before the UI
                // starts reading the session or the cache.
                pendingUnlockPassword = pw
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
        appInForeground = true
        // Back in the foreground: the in-app UI now shows messages, so clear any
        // pending "new message" notification + its running count.
        ThefeedService.pendingNewCount = 0
        try {
            NotificationManagerCompat.from(this).cancel(ThefeedService.MSG_NOTIFICATION_ID)
        } catch (_: Exception) {
        }
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

    override fun onPause() {
        super.onPause()
        appInForeground = false
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
            // The local server runs with HTTP Basic under a per-launch secret,
            // so this is what lets our own WebView in while every other app on
            // the device gets 401. It fires for sub-resources as well as the
            // page, which is why nothing else -- no header on loadUrl, no
            // change anywhere in the frontend -- has to carry the secret.
            override fun onReceivedHttpAuthRequest(
                view: WebView?,
                handler: HttpAuthHandler?,
                host: String?,
                realm: String?
            ) {
                val secret = getLocalSecret()
                if (handler == null) return
                // Only ever to our own loopback server, and never a second
                // time for the same request: proceed() on a retry would mean
                // the secret is wrong, and looping would hammer it.
                if (secret.isEmpty() || host != "127.0.0.1" || handler.useHttpAuthUsernamePassword()) {
                    handler.cancel()
                    return
                }
                handler.proceed("thefeed", secret)
            }

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

            override fun onRenderProcessGone(
                view: WebView?,
                detail: RenderProcessGoneDetail?
            ): Boolean {
                // The WebView renderer process died (OOM kill or crash — common
                // under aggressive OEM process management, e.g. MIUI). The dead
                // WebView can only render a blank/black surface from this point
                // on; the default (unhandled) behavior kills the whole app.
                // Recreate the WebView and reload instead.
                if (view === webView) {
                    handler.post { recreateWebView() }
                } else {
                    view?.destroy()
                }
                return true
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
                // acceptTypes returns [""] (not an empty array) when no accept attribute
                // is set on the input — filter blanks so we never pass an empty MIME
                // type to OpenDocument, which would crash on Android.
                val types = fileChooserParams?.acceptTypes
                    ?.filter { it.isNotBlank() }
                    ?.toTypedArray()
                    ?.takeIf { it.isNotEmpty() } ?: arrayOf("*/*")
                fileChooserLauncher.launch(types)
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

            override fun onShowCustomView(view: View?, callback: CustomViewCallback?) {
                if (view == null || fullscreenView != null) {
                    callback?.onCustomViewHidden()
                    return
                }
                fullscreenView = view
                fullscreenCallback = callback
                val decor = window.decorView as android.view.ViewGroup
                decor.addView(view, android.view.ViewGroup.LayoutParams(
                    android.view.ViewGroup.LayoutParams.MATCH_PARENT,
                    android.view.ViewGroup.LayoutParams.MATCH_PARENT))
                WindowCompat.getInsetsController(window, view).apply {
                    systemBarsBehavior = WindowInsetsControllerCompat.BEHAVIOR_SHOW_TRANSIENT_BARS_BY_SWIPE
                    hide(WindowInsetsCompat.Type.systemBars())
                }
            }

            override fun onHideCustomView() {
                hideFullscreenView()
            }
        }

        // One-time cache flush on app update. The pinned localhost port keeps the
        // WebView's asset URLs stable, so across an app update it could serve a
        // STALE cached bundle (old JS vs. the new index.html) — the mismatch that
        // blanked the screen. Clear it once per version change; normal caching
        // resumes afterward (media stays cached, no flicker). clearCache does NOT
        // touch domStorage (settings).
        run {
            val prefs = getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
            if (prefs.getInt("webCacheVersion", -1) != BuildConfig.VERSION_CODE) {
                webView.clearCache(true)
                prefs.edit().putInt("webCacheVersion", BuildConfig.VERSION_CODE).apply()
            }
        }
        with(webView.settings) {
            javaScriptEnabled = true
            domStorageEnabled = true
            // Respect HTTP cache headers: code assets are no-store (always fresh),
            // media caches normally for smooth scrolling.
            cacheMode = WebSettings.LOAD_DEFAULT
            allowFileAccess = false
            allowContentAccess = false
            mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
        }

        webView.addJavascriptInterface(AndroidBridge(this), "Android")
    }

    /**
     * Polls SharedPreferences for the port on every attempt, then probes
     * the URL. Status text stays hidden for the first QUIET_ATTEMPTS so
     * the common in-process-gomobile case (server up in <1 s) doesn't
     * flash a counter at the user. Only slow starts or outright failures
     * surface a message.
     */
    private fun waitForServerThenLoad() {
        setStatus("")
        Thread {
            var ready = false
            var lastPort = -1
            for (attempt in 1..MAX_PROBE_ATTEMPTS) {
                val port = getCurrentPort()
                if (port <= 0) {
                    if (attempt > QUIET_ATTEMPTS) {
                        handler.post { setStatus("Waiting for service... ($attempt/$MAX_PROBE_ATTEMPTS)") }
                    }
                    Thread.sleep(PROBE_INTERVAL_MS)
                    continue
                }
                if (port != lastPort) {
                    lastPort = port
                    if (attempt > QUIET_ATTEMPTS) {
                        handler.post { setStatus("Connecting...") }
                    }
                }
                try {
                    val conn = URL("http://127.0.0.1:$port").openConnection() as HttpURLConnection
                    conn.connectTimeout = PROBE_TIMEOUT_MS.toInt()
                    conn.readTimeout = PROBE_TIMEOUT_MS.toInt()
                    conn.requestMethod = "GET"
                    val code = conn.responseCode
                    conn.disconnect()
                    if (code > 0) {
                        // The server runs with HTTP Basic under a secret
                        // generated this launch; onReceivedHttpAuthRequest
                        // below answers the challenge, for sub-resources too.
                        // Nothing is appended to the URL.
                        val secret = getLocalSecret()
                        if (secret.isEmpty()) {
                            Thread.sleep(PROBE_INTERVAL_MS)
                            continue
                        }
                        // Open the sealed Telegram store before the page
                        // loads. Already on a background thread, which is
                        // where the Argon2id derivation belongs. A failure is
                        // not fatal -- the section reports itself locked
                        // rather than the app refusing to start.
                        pendingUnlockPassword?.let { pw ->
                            pendingUnlockPassword = null
                            if (!AndroidBridge(this@MainActivity).telUnlock(pw)) {
                                android.util.Log.w("thefeed", "telegram store stayed locked")
                            }
                        }
                        ready = true
                        val url = "http://127.0.0.1:$port"
                        handler.post { setStatus(""); webView.loadUrl(url) }
                        return@Thread
                    }
                } catch (_: Exception) {
                    // Connection refused — not ready yet
                }
                if (attempt > QUIET_ATTEMPTS) {
                    handler.post { setStatus("Waiting for server... ($attempt/$MAX_PROBE_ATTEMPTS)") }
                }
                Thread.sleep(PROBE_INTERVAL_MS)
            }
            if (!ready) {
                handler.post { setStatus("Could not connect. Restart the app.") }
            }
        }.start()
    }

    // The passcode, between a successful unlock and the server being up.
    // Cleared as soon as it has been handed over.
    private var pendingUnlockPassword: String? = null

    private fun getLocalSecret(): String {
        val prefs = getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
        return prefs.getString(ThefeedService.PREF_LOCAL_SECRET, "") ?: ""
    }

    private fun getCurrentPort(): Int {
        val prefs = getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
        return prefs.getInt(ThefeedService.PREF_PORT, -1)
    }

    // Exits HTML5 video fullscreen: removes the overlay view, restores the
    // system bars, and notifies the WebView. Safe to call when not fullscreen.
    private fun hideFullscreenView() {
        val v = fullscreenView ?: return
        val decor = window.decorView as android.view.ViewGroup
        decor.removeView(v)
        WindowCompat.getInsetsController(window, webView).show(WindowInsetsCompat.Type.systemBars())
        fullscreenView = null
        fullscreenCallback?.onCustomViewHidden()
        fullscreenCallback = null
    }

    // Replaces a WebView whose renderer process died with a fresh instance
    // (same id, position, and layout params) and restarts the load cycle.
    private fun recreateWebView() {
        // Drop any orphaned fullscreen video overlay from the dead renderer
        hideFullscreenView()
        val parent = webView.parent as? android.view.ViewGroup ?: return
        val index = parent.indexOfChild(webView)
        val params = webView.layoutParams
        val wasVisible = webView.visibility
        parent.removeView(webView)
        webView.destroy()
        webView = WebView(this)
        webView.id = R.id.webView
        webView.visibility = wasVisible
        parent.addView(webView, index, params)
        configureWebView()
        if (!lockScreenVisible) waitForServerThenLoad()
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
        // Suppress the "Waiting…" counter for the first N probes — with
        // in-process gomobile the server is up in <1 s and showing a
        // counter for that brief window just looks broken.
        private const val QUIET_ATTEMPTS    = 3
        private const val PREF_BATTERY_OPT_DECLINED = "battery_opt_declined"

        // Read by ThefeedService (from a Go poll thread) to decide whether a new
        // message warrants a system notification — foreground alerts are the web
        // UI's job, so the service only notifies while the app is backgrounded.
        @Volatile
        var appInForeground = false
    }
}

