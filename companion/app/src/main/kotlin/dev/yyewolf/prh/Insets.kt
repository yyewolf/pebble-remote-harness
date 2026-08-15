package dev.yyewolf.prh

import android.view.View
import androidx.core.view.ViewCompat
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
fun View.padForSystemBars() {
    val start = Rect4(paddingLeft, paddingTop, paddingRight, paddingBottom)
    ViewCompat.setOnApplyWindowInsetsListener(this) { v, windowInsets ->
        val bars = windowInsets.getInsets(
            WindowInsetsCompat.Type.systemBars() or WindowInsetsCompat.Type.displayCutout(),
        )
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

/** The view's padding as it was before any insets were folded in. */
private data class Rect4(val left: Int, val top: Int, val right: Int, val bottom: Int)
