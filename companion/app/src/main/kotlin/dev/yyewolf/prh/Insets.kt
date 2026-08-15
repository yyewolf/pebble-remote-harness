package dev.yyewolf.prh

import android.app.Activity
import android.content.res.Configuration
import android.view.View
import androidx.core.view.ViewCompat
import androidx.core.view.WindowCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.updatePadding

/**
 * Insets the view below the status bar and above the navigation bar.
 *
 * Since targetSdk 35 Android draws every app edge-to-edge and no longer lets
 * one opt out, so a view laid out at y=0 really does start behind the status
 * bar. Nothing here used to account for that: the top of every screen was
 * covered, which on the session list meant the first row was invisible and the
 * list appeared to be one session short of what prh was serving.
 *
 * The view's own padding is preserved and the insets are added to it, so this
 * can be applied to a root that already has padding of its own.
 */
fun View.padForSystemBars(includeIme: Boolean = false) {
    val start = Rect4(paddingLeft, paddingTop, paddingRight, paddingBottom)
    ViewCompat.setOnApplyWindowInsetsListener(this) { v, windowInsets ->
        var types = WindowInsetsCompat.Type.systemBars() or WindowInsetsCompat.Type.displayCutout()
        if (includeIme) types = types or WindowInsetsCompat.Type.ime()
        val bars = windowInsets.getInsets(types)
        v.updatePadding(
            left = start.left + bars.left,
            top = start.top + bars.top,
            right = start.right + bars.right,
            bottom = start.bottom + bars.bottom,
        )
        // Consume nothing: children may want the insets too.
        windowInsets
    }
    ViewCompat.requestApplyInsets(this)
}

/**
 * Picks dark or light system-bar icons to suit the current theme.
 *
 * The bars are transparent and the app draws underneath them, so the icons
 * sit directly on the app's own background. Left alone they stay light, which
 * is invisible against the light palette — the clock and battery simply
 * vanish from the top of every screen.
 */
fun Activity.matchSystemBarsToTheme() {
    val night = (resources.configuration.uiMode and Configuration.UI_MODE_NIGHT_MASK) ==
        Configuration.UI_MODE_NIGHT_YES
    WindowCompat.getInsetsController(window, window.decorView).apply {
        isAppearanceLightStatusBars = !night
        isAppearanceLightNavigationBars = !night
    }
}

/** The view's padding as it was before any insets were folded in. */
private data class Rect4(val left: Int, val top: Int, val right: Int, val bottom: Int)
