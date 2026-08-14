# Android companion

The companion is what makes app-closed prompts work. It is the only component
that can wake a closed watchapp without user interaction.

## Verify this first

Everything here depends on one unverified assumption:

> The Core Devices Pebble mobile app still implements the classic PebbleKit
> Android intent surface.

Classic PebbleKit Android is not a network protocol — it is a thin wrapper over
Android intents aimed at the official Pebble app:

- `com.getpebble.action.app.START` / `.STOP` — launch or close a watchapp
- `com.getpebble.action.SEND_DATA` — AppMessage out
- `com.getpebble.action.app.RECEIVE` — AppMessage in
- a content provider at `com.getpebble.provider` for connection state

The Core Devices app is a rewrite. It may keep this surface for compatibility,
it may expose something new, or it may expose nothing. **Do not write the
service until you know.** The probe, in order:

1. Pull the APK off the phone and read its manifest for exported receivers and
   the `com.getpebble.*` action strings:
   ```bash
   adb shell pm path <pebble.package.name>
   adb pull <apk path> pebble.apk
   # then: apkanalyzer manifest print pebble.apk   (or aapt2 dump xmltree)
   ```
2. Failing that, read the Core Devices mobile app source — it is open source.
3. Cheapest empirical test: a 50-line throwaway Android app that calls
   `PebbleKit.startAppOnPebble(ctx, UUID)` against a sideloaded watchapp. If
   the watchapp opens, the assumption holds and the rest is ordinary work.

If it does not hold, fall back to `docs/notifications.md` and move the
transport back into `watchapp/src/pkjs/`.

## Responsibilities

| Concern | Detail |
|---|---|
| Transport | Foreground service holding `GET /v1/poll` against `prh`, with backoff and cursor persistence |
| Wake | `PebbleKit.startAppOnPebble(ctx, WATCHAPP_UUID)` on `perm` and `ques` envelopes |
| Deliver | `PebbleKit.sendDataToPebble()` with the AppMessage dictionary from `protocol.md` |
| Reply | `PebbleKit.registerReceivedDataHandler()` -> `POST /v1/reply` |
| Register | Settings screen: host, port, password. Calls `/v1/register`, stores the device token in `EncryptedSharedPreferences` |
| Fallback | Post an Android notification when the watch is disconnected, so the prompt is not silently lost |

## Battery and lifecycle

A permanently-held long-poll on Android needs care:

- Foreground service with a low-importance ongoing notification. Required on
  API 26+, and it is what keeps the socket alive.
- Request battery-optimisation exemption. Doze will otherwise kill the poll
  and prompts will arrive minutes late or not at all.
- The `prh` side should cap the poll wait at ~55s, well under typical NAT and
  carrier idle timeouts.
- On poll timeout, reconnect immediately with the same cursor. On error, back
  off exponentially to a ceiling of ~60s.
- `START_STICKY` plus a boot receiver, so the service returns after a reboot.

## Scope of this scaffold

`companion/` contains layout and stubs only. There is no JDK, Gradle, or
Android SDK in this checkout, so nothing here has been compiled. Before the
first real commit of Android code:

1. Run the probe above.
2. Pin the PebbleKit dependency — the classic artifact is
   `com.getpebble:pebblekit:4.0.1`, but check whether Core Devices publishes a
   maintained fork.
3. Decide Kotlin vs Java. The stubs assume Kotlin.
