pluginManagement {
    repositories {
        google()
        mavenCentral()
        gradlePluginPortal()
    }
}

dependencyResolutionManagement {
    repositories {
        google()
        mavenCentral()
        // PebbleKit Android was historically published to JitPack rather than
        // Maven Central. Verify where a maintained artifact lives before
        // relying on this — see docs/android-companion.md.
        maven { url = uri("https://jitpack.io") }
    }
}

rootProject.name = "prh-companion"
include(":app")
