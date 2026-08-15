package dev.yyewolf.prh

import android.content.Context
import android.content.res.ColorStateList
import android.graphics.Typeface
import android.graphics.drawable.Drawable
import android.graphics.drawable.GradientDrawable
import android.graphics.drawable.RippleDrawable
import android.text.InputType
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.Switch
import android.widget.TextView
import androidx.core.content.ContextCompat

/**
 * The companion's look, in one file.
 *
 * Every screen here is built in code rather than XML, which is fine for a
 * six-screen app but means there is nothing stopping each one inventing its
 * own paddings and greys — which is exactly what had happened. This is the
 * shared vocabulary: colours come from resources so light and dark are a
 * qualifier rather than a branch, sizes come from [dp] so they are the same
 * physical size on every screen, and the handful of recurring shapes (a card,
 * a pill, a filled button) are built once.
 *
 * No Material Components dependency. The library would bring a second theming
 * system and a large transitive tree for what amounts to five drawables, and
 * the build file is already on record preferring self-contained over
 * convenient — see the note about ML Kit there.
 */

// -- units ----------------------------------------------------------------

/** Density-independent pixels. Raw pixel paddings were the reason nothing
 *  lined up: 20px is a comfortable gap on a cheap phone and a hairline on a
 *  modern one. */
fun Context.dp(value: Int): Int = (value * resources.displayMetrics.density + 0.5f).toInt()

fun Context.dpf(value: Float): Float = value * resources.displayMetrics.density

// -- palette ---------------------------------------------------------------

private fun Context.col(id: Int) = ContextCompat.getColor(this, id)

val Context.bgColor: Int get() = col(R.color.bg)
val Context.surfaceColor: Int get() = col(R.color.surface)
val Context.surfaceAltColor: Int get() = col(R.color.surface_alt)
val Context.borderColor: Int get() = col(R.color.border)
val Context.textPrimary: Int get() = col(R.color.text_primary)
val Context.textSecondary: Int get() = col(R.color.text_secondary)
val Context.textFaint: Int get() = col(R.color.text_faint)
val Context.accentColor: Int get() = col(R.color.accent)
val Context.accentSoft: Int get() = col(R.color.accent_soft)
val Context.onAccent: Int get() = col(R.color.on_accent)
val Context.goodColor: Int get() = col(R.color.good)
val Context.goodSoft: Int get() = col(R.color.good_soft)
val Context.warnColor: Int get() = col(R.color.warn)
val Context.badColor: Int get() = col(R.color.bad)
val Context.badSoft: Int get() = col(R.color.bad_soft)
val Context.rippleColor: Int get() = col(R.color.ripple)

// -- type ------------------------------------------------------------------

private val MEDIUM: Typeface? = Typeface.create("sans-serif-medium", Typeface.NORMAL)
private val MONO: Typeface? = Typeface.create("monospace", Typeface.NORMAL)

/** The screen's name. One per screen, at the top, and nothing else this big. */
fun Context.titleView(text: CharSequence): TextView = TextView(this).apply {
    this.text = text
    textSize = 26f
    typeface = MEDIUM
    setTextColor(textPrimary)
    letterSpacing = -0.02f
}

/** A group heading. Small, spaced, muted — it labels, it does not compete. */
fun Context.sectionLabel(text: CharSequence): TextView = TextView(this).apply {
    this.text = text.toString().uppercase()
    textSize = 11f
    typeface = MEDIUM
    letterSpacing = 0.12f
    setTextColor(textFaint)
}

fun Context.bodyView(text: CharSequence = ""): TextView = TextView(this).apply {
    this.text = text
    textSize = 15f
    setTextColor(textPrimary)
    setLineSpacing(dpf(3f), 1f)
}

fun Context.captionView(text: CharSequence = ""): TextView = TextView(this).apply {
    this.text = text
    textSize = 13f
    setTextColor(textSecondary)
    setLineSpacing(dpf(2f), 1f)
}

/** For anything the agent is quoting verbatim: commands, paths, diffs. */
fun Context.monoView(text: CharSequence = ""): TextView = TextView(this).apply {
    this.text = text
    textSize = 13f
    typeface = MONO
    setTextColor(textPrimary)
    setLineSpacing(dpf(2f), 1f)
}

// -- shapes ----------------------------------------------------------------

fun Context.roundedRect(
    fill: Int,
    radiusDp: Int = 14,
    strokeColor: Int? = null,
    strokeDp: Int = 1,
): GradientDrawable = GradientDrawable().apply {
    shape = GradientDrawable.RECTANGLE
    cornerRadius = dpf(radiusDp.toFloat())
    setColor(fill)
    if (strokeColor != null) setStroke(dp(strokeDp), strokeColor)
}

/** Wraps a shape so it responds to touch. Without this, every custom
 *  background silently kills the press feedback the stock widget had. */
fun Context.withRipple(content: Drawable): Drawable =
    RippleDrawable(ColorStateList.valueOf(rippleColor), content, null)

/** A raised panel: the unit every screen is composed of. */
fun Context.card(padding: Int = 16): LinearLayout = LinearLayout(this).apply {
    orientation = LinearLayout.VERTICAL
    background = roundedRect(surfaceColor, strokeColor = borderColor)
    setPadding(dp(padding), dp(padding), dp(padding), dp(padding))
}

/** A small status badge: "waiting", "asking", "paired". */
fun Context.pill(text: CharSequence, fg: Int, bg: Int): TextView = TextView(this).apply {
    this.text = text.toString().uppercase()
    textSize = 10f
    typeface = MEDIUM
    letterSpacing = 0.08f
    setTextColor(fg)
    background = roundedRect(bg, radiusDp = 20)
    setPadding(dp(8), dp(4), dp(8), dp(4))
}

// -- controls --------------------------------------------------------------

private fun Context.baseButton(label: CharSequence): Button = Button(this).apply {
    text = label
    textSize = 15f
    typeface = MEDIUM
    isAllCaps = false
    // The platform Button carries a shadow animator that fights a flat custom
    // background: without clearing it the corners lift and shade on press.
    stateListAnimator = null
    minHeight = dp(48)
    minimumHeight = dp(48)
    setPadding(dp(20), dp(12), dp(20), dp(12))
}

/** The one action a screen most wants you to take. At most one per screen. */
fun Context.primaryButton(label: CharSequence): Button = baseButton(label).apply {
    background = withRipple(roundedRect(accentColor))
    setTextColor(onAccent)
}

/** Everything else: outlined, so it reads as available but not urgent. */
fun Context.secondaryButton(label: CharSequence): Button = baseButton(label).apply {
    background = withRipple(roundedRect(surfaceColor, strokeColor = borderColor))
    setTextColor(textPrimary)
}

/** Approve. Green is load-bearing here — it is the only affirmative button. */
fun Context.positiveButton(label: CharSequence): Button = baseButton(label).apply {
    background = withRipple(roundedRect(goodSoft, strokeColor = goodColor))
    setTextColor(goodColor)
}

/** Reject. */
fun Context.negativeButton(label: CharSequence): Button = baseButton(label).apply {
    background = withRipple(roundedRect(badSoft, strokeColor = badColor))
    setTextColor(badColor)
}

fun Context.styledField(hint: CharSequence, multiline: Boolean = false): EditText =
    EditText(this).apply {
        this.hint = hint
        textSize = 15f
        setTextColor(textPrimary)
        setHintTextColor(textFaint)
        background = roundedRect(surfaceAltColor, strokeColor = borderColor)
        setPadding(dp(14), dp(12), dp(14), dp(12))
        inputType = if (multiline) {
            InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_FLAG_MULTI_LINE
        } else {
            InputType.TYPE_CLASS_TEXT
        }
        if (!multiline) maxLines = 1
    }

/**
 * A settings row: label, explanation, switch.
 *
 * A Switch rather than a CheckBox because this writes straight through to
 * prefs and takes effect on the next prompt — there is no form to submit, and
 * a switch is the control that says so. The whole row is the touch target;
 * a 24dp checkbox is not a comfortable thing to hit on a phone.
 */
fun Context.switchRow(
    label: CharSequence,
    explanation: CharSequence,
    checked: Boolean,
    onChange: (Boolean) -> Unit,
): LinearLayout {
    val toggle = Switch(this).apply {
        isChecked = checked
        // Assigned before the listener, deliberately: attaching first fires the
        // callback for the value just read back out of prefs and writes it
        // straight in again, which is how a setting ends up "changed" by
        // merely opening the screen.
        setOnCheckedChangeListener { _, value -> onChange(value) }
        // The off thumb is a mid grey, not the card colour: tinted to the
        // surface it sits on it reads as "no switch here" in dark mode rather
        // than as a switch that is off.
        thumbTintList = ColorStateList(
            arrayOf(intArrayOf(android.R.attr.state_checked), intArrayOf()),
            intArrayOf(accentColor, textFaint),
        )
        trackTintList = ColorStateList(
            arrayOf(intArrayOf(android.R.attr.state_checked), intArrayOf()),
            intArrayOf(accentSoft, borderColor),
        )
    }

    val text = LinearLayout(this).apply {
        orientation = LinearLayout.VERTICAL
        layoutParams = LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f)
        addView(
            bodyView(label).apply {
                typeface = MEDIUM
                textSize = 15f
            },
        )
        addView(captionView(explanation).apply { setPadding(0, dp(2), 0, 0) })
    }

    return LinearLayout(this).apply {
        orientation = LinearLayout.HORIZONTAL
        gravity = Gravity.CENTER_VERTICAL
        setPadding(0, dp(10), 0, dp(10))
        addView(text)
        addView(
            toggle,
            LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT,
                ViewGroup.LayoutParams.WRAP_CONTENT,
            ).apply { leftMargin = dp(12) },
        )
        // Tapping anywhere on the row, not just the 40dp switch.
        setOnClickListener { toggle.toggle() }
    }
}

/** A hairline between rows inside a card. */
fun Context.divider(): View = View(this).apply {
    setBackgroundColor(borderColor)
    layoutParams = LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, dp(1))
}

// -- layout helpers --------------------------------------------------------

fun matchWrap(): LinearLayout.LayoutParams = LinearLayout.LayoutParams(
    ViewGroup.LayoutParams.MATCH_PARENT,
    ViewGroup.LayoutParams.WRAP_CONTENT,
)

/** Fills the leftover vertical space. Height 0 with a weight, never
 *  WRAP_CONTENT — a scrolling child asked to wrap measures a sample of its
 *  rows and resizes as the data changes. */
fun matchRest(): LinearLayout.LayoutParams = LinearLayout.LayoutParams(
    ViewGroup.LayoutParams.MATCH_PARENT,
    0,
    1f,
)

/** Adds a child at full width with a top margin, the common case. */
fun LinearLayout.addSpaced(child: View, topDp: Int) {
    val params = (child.layoutParams as? LinearLayout.LayoutParams) ?: matchWrap()
    params.topMargin = context.dp(topDp)
    addView(child, params)
}
