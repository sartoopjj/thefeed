package com.thefeed.android

import android.app.Activity
import android.content.Context
import android.webkit.JavascriptInterface
import java.security.MessageDigest

class AndroidBridge(private val activity: Activity) {

    private val prefs by lazy {
        activity.getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
    }

    // ===== Identity =====

    @JavascriptInterface
    fun isAndroid(): Boolean = true

    /** Set a preset display name for the app (shown on lock screen). */
    @JavascriptInterface
    fun setPresetName(name: String) {
        prefs.edit().putString(PREF_CUSTOM_APP_NAME, name).apply()
    }

    /** Returns the preset display name, or empty string if using defaults. */
    @JavascriptInterface
    fun getPresetName(): String {
        return prefs.getString(PREF_CUSTOM_APP_NAME, "") ?: ""
    }

    /** Returns the display name for the app — preset name if set, otherwise "thefeed". */
    @JavascriptInterface
    fun getAppDisplayName(): String {
        val custom = prefs.getString(PREF_CUSTOM_APP_NAME, null)
        return if (!custom.isNullOrBlank()) custom else "thefeed"
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

    private fun sha256(input: String): String {
        val digest = MessageDigest.getInstance("SHA-256")
        val hash = digest.digest(input.toByteArray(Charsets.UTF_8))
        return hash.joinToString("") { "%02x".format(it) }
    }

    companion object {
        const val PREF_CUSTOM_APP_NAME = "custom_app_name"
        const val PREF_PASSWORD_HASH = "password_hash"
        const val PREF_LANG = "app_lang"
    }
}
