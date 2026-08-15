// Verified against Google Maven, built with Gradle 9.7.0 on JDK 17.
//
// No org.jetbrains.kotlin.android plugin: AGP 9.0+ has built-in Kotlin
// support and rejects the standalone plugin outright.
plugins {
    id("com.android.application") version "9.3.1" apply false
}
