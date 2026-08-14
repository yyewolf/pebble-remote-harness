# Toolchain

Every component has been built at least once. This records the versions that
worked and where they live, because none of it came from system packages —
all of it installs into `$HOME` without root.

| Component | Needs | Version verified |
|---|---|---|
| `api/` | Go | 1.26.4 |
| `extension/` | Node, npm | 24.14.1 / 11.11.0 |
| `watchapp/` | pebble-tool + SDK | 5.0.39 / SDK 4.33.1 |
| `companion/` | JDK, Android SDK, Gradle | 17.0.20 / build-tools 36.0.0 / 9.7.0 |

## Install locations

```
~/.local/bin/pebble                     pebble-tool (uv tool install)
~/.local/share/pebble-sdk/SDKs/4.33.1   Pebble SDK + ARM toolchain
~/.local/share/jdk/jdk-17.0.20+8        Temurin JDK 17
~/Android/Sdk                           Android SDK
~/.local/share/gradle/gradle-9.7.0      Gradle (only to bootstrap the wrapper)
```

Gradle is only needed once, to generate `companion/gradlew`. After that the
wrapper is the entry point and pins its own version.

## Reproducing

```bash
# Pebble
uv tool install --python 3.12 pebble-tool
pebble sdk install 4.33.1

# JDK (no root)
mkdir -p ~/.local/share/jdk && cd ~/.local/share/jdk
curl -sL "https://api.adoptium.net/v3/binary/latest/17/ga/linux/x64/jdk/hotspot/normal/eclipse" | tar xz

# Android SDK — note the filename is "commandlinetools", not
# "commandline-tools"; the hyphenated guess 404s.
mkdir -p ~/Android/Sdk/cmdline-tools
curl -sLO https://dl.google.com/android/repository/commandlinetools-linux-15859902_latest.zip
unzip -q commandlinetools-linux-*_latest.zip
mv cmdline-tools ~/Android/Sdk/cmdline-tools/latest

export JAVA_HOME=~/.local/share/jdk/jdk-17.0.20+8
export ANDROID_HOME=~/Android/Sdk
export PATH="$JAVA_HOME/bin:$ANDROID_HOME/cmdline-tools/latest/bin:$PATH"
yes | sdkmanager --licenses
sdkmanager "platform-tools" "platforms;android-36" "build-tools;36.0.0"
```

To find the current command-line tools build number rather than guessing,
parse Google's manifest:

```bash
curl -s https://dl.google.com/android/repository/repository2-3.xml \
  | grep -o 'commandlinetools-linux-[0-9]*_latest.zip' | head -1
```

## Gotchas hit while setting this up

- **AGP 9 has built-in Kotlin support.** Applying
  `org.jetbrains.kotlin.android` alongside it is a hard error, not a warning:
  *"The 'org.jetbrains.kotlin.android' plugin is no longer required for Kotlin
  support since AGP 9.0."* The root build file carries only the AGP plugin.
- **`companion/local.properties`** holds `sdk.dir` and is gitignored. A fresh
  clone needs it, or `ANDROID_HOME` set in the environment.
- **pebble-tool wants Python ≤3.12.** The system Python here is 3.14, so the
  install pins `--python 3.12`.
- **`emery` vs `gabbro`.** Both are modern platforms in SDK 4.33.1. The Pebble
  Time 2 is `emery`; gabbro is 260x260 and round.

## Build everything

```bash
make api extension watchapp companion
```

The Makefile defaults `JAVA_HOME` and `ANDROID_HOME` to the paths above, so
it works without touching your shell profile. Override either if you install
them elsewhere.
