// Kotlin support comes from AGP itself — see the note in the root build file.
plugins {
    id("com.android.application")
}

android {
    namespace = "dev.yyewolf.prh"
    compileSdk = 36

    defaultConfig {
        applicationId = "dev.yyewolf.prh"
        // 26 is the floor: foreground services are what keep the long-poll
        // alive, and they are an API 26 concept.
        minSdk = 26
        targetSdk = 36
        versionCode = 1
        versionName = "0.1.0"
    }

    buildTypes {
        release {
            isMinifyEnabled = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

dependencies {
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("androidx.security:security-crypto:1.1.0-alpha06")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.8.1")

    // Classic PebbleKit, still on Maven Central (4.0.1 is the latest and
    // final release). Verified to resolve and to land com.getpebble.android.kit
    // classes in the APK, and the Core Devices app implements the intent
    // surface it drives — see docs/android-companion.md.
    //
    // Unmaintained by definition, but it is a thin wrapper over five intents
    // and a content provider: if it ever breaks, reimplementing it directly is
    // a day's work, not a redesign.
    implementation("com.getpebble:pebblekit:4.0.1")

    // In-app QR scan for pairing. The pairing code is a `prh://` deep link,
    // and stock camera apps only auto-open http(s): they show the unknown
    // scheme as text and never fire a VIEW intent. So the companion scans the
    // code itself and hands the decoded URI straight to parseDeepLink — the
    // manifest intent filter still routes a real deep link, but pairing no
    // longer depends on the scanner honoring a custom scheme.
    //
    // zxing-android-embedded over ML Kit: ML Kit pulls Google Play Services,
    // which is not a safe assumption on the kind of device that still talks
    // to a Pebble, and the whole point is an offline, self-contained flow.
    implementation("com.journeyapps:zxing-android-embedded:4.3.0")
}
