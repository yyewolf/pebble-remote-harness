plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "dev.yyewolf.prh"
    compileSdk = 35

    defaultConfig {
        applicationId = "dev.yyewolf.prh"
        // 26 is the floor: foreground services are what keep the long-poll
        // alive, and they are an API 26 concept.
        minSdk = 26
        targetSdk = 35
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

    kotlinOptions {
        jvmTarget = "17"
    }
}

dependencies {
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("androidx.security:security-crypto:1.1.0-alpha06")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.8.1")

    // TODO: unpinned on purpose. The classic artifact is
    // com.getpebble:pebblekit:4.0.1, but check whether Core Devices publishes
    // a maintained fork before wiring this up.
    // implementation("com.getpebble:pebblekit:4.0.1")
}
