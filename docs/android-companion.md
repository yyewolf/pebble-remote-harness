# Android companion

The companion is what makes app-closed prompts work. It is the only component
that can wake a closed watchapp without user interaction.

## Compatibility: verified

> **The Core Devices app supports classic PebbleKit.** Probed against
> `coredevices.coreapp` 1.8.0.7 on a Pixel 8 Pro. The companion design holds.

The manifest alone is misleading — it declares only providers, no receivers,
which reads like the classic surface is gone. It is not: the app registers the
receivers **at runtime**, so they never appear in the manifest. The dex
carries the full set:

```
com.getpebble.action.app.START          ← launches a closed watchapp
com.getpebble.action.app.STOP
com.getpebble.action.app.SEND
com.getpebble.action.app.RECEIVE
com.getpebble.action.app.ACK / NACK / RECEIVE_ACK / RECEIVE_NACK
```

alongside class names `PebbleKitClassicStartListeners` and
`io.rebble.libpebblecommon.pebblekit.classic.PebbleKitProvider`.

Runtime confirmation — querying the classic provider on a live phone:

```console
$ adb shell content query --uri content://com.getpebble.android.provider.basalt/state
Row: 0 0=1, 1=1, 2=0, 3=4, 4=33, 5=2, 6=
```

Decoded against PebbleKit's column order: **connected=1**, **AppMessage
support=1**, datalogging=0, firmware **4.33.2** — which lines up with the
SDK 4.33.1 the watchapp is built against.

Implicit broadcasts still reach runtime-registered receivers on Android 8+;
the ban applies to manifest-declared ones. So `startAppOnPebble` works by the
ordinary mechanism.

### Package name has changed

The classic package `com.getpebble.android.basalt` appears in the dex, but the
**installed package is `coredevices.coreapp`**. Android 11+ package-visibility
rules mean the companion's `<queries>` must name the real package, or the
provider lookup returns nothing on a modern target SDK.

### PebbleKit 2 exists, and is the forward-looking API

The app also ships a second, modern surface:

```
provider  io.rebble.libpebblecommon.pebblekit.two.PebbleKitProvider
          authority coredevices.coreapp.pebblekit          (exported)
service   io.rebble.libpebblecommon.pebblekit.two.PebbleSenderReceiver
          action io.rebble.pebblekit2.SEND_DATA_TO_WATCH   (exported)
broadcast io.rebble.pebblekit2.RECEIVE_DATA_FROM_WATCH
aidl      io.rebble.pebblekit2.common.UniversalRequestResponse
```

It is a bound service with an AIDL interface rather than a broadcast dance.
Two caveats: its provider returned `No result found` for a `/state` query, so
the URI layout differs from classic; and **no app-launch operation is visible
in it** — only send and receive. Classic is currently the surface with
`START`, which is the one capability this project cannot do without.

Use classic, watch PebbleKit 2. If classic is eventually removed, confirm
PebbleKit 2 can launch a watchapp before assuming the migration is
mechanical — if it cannot, the fallbacks in `notifications.md` apply.

### Still to prove end to end

Reading the surface is not the same as exercising it. The remaining test is to
sideload `watchapp.pbw`, fire `com.getpebble.action.app.START` with our UUID,
and watch the app open on the wrist.

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
