# Waking the watch

## The constraint

PebbleKit JS runs inside the Pebble mobile app's JS runtime and is started and
killed with the watchapp. There is no background JS and no background
networking on Pebble. A watchapp that is closed cannot be listening.

So something off-watch has to do the waking.

## Chosen path: Android companion

`PebbleKit.startAppOnPebble(ctx, WATCHAPP_UUID)` launches a closed watchapp
programmatically. An Android foreground service can hold the long-poll
indefinitely, so a permission request reaches the wrist with nothing open and
nothing tapped.

This is the only option that is genuinely automatic. It costs a fourth
component and it is Android-only. See `docs/android-companion.md`, including
the compatibility probe that must pass first.

## Fallbacks, if the companion path fails

Kept here because the PebbleKit-on-Core-Devices question is unresolved.

### Timeline pins

`prh` pushes a pin to the timeline web service; the watch shows it with the
app closed, and an `openWatchApp` pin action launches the harness.

- Outbound-only: the dev box calls the timeline API, nothing needs to reach
  the dev box. That is a real advantage over anything push-based.
- Needs a per-user-per-app timeline token from `Pebble.getTimelineToken()`.
- **Unverified:** whether Rebble still issues timeline tokens for unpublished,
  sideloaded apps. Historically developer sandbox tokens worked. Check before
  committing to this.
- Wakes on tap, not automatically.

### Phone notification mirroring

`prh` publishes to an ntfy topic; the Android ntfy client posts a
notification; the Pebble app mirrors it to the watch.

- Works today, no Pebble web services, no custom Android code.
- ntfy `http` action buttons can call `/v1/reply` directly, so approve/deny
  from the wrist is possible without the watchapp at all.
- Coarse: no watchapp UI, no dictation, and the payload is whatever fits in a
  notification.

## Foreground behaviour is unaffected

With the watchapp open, none of this matters — the companion (or pkjs, in the
fallback design) streams envelopes straight in. Waking is only about the
closed-app case.

## Implementation seam

`api/internal/waker` defines a `Waker` interface with a no-op default, so
timeline and ntfy can be added without touching the hub. The companion path
needs no waker at all: it is the companion that does the waking, and `prh`
only has to deliver the envelope.
