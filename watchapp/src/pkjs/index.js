// PebbleKit JS — fallback transport only.
//
// In the chosen design this file does almost nothing: the Android companion
// holds the long-poll and pushes envelopes over PebbleKit Android, because
// pkjs is killed whenever the watchapp closes and so cannot wake anything.
//
// It exists for the case where the PebbleKit-on-Core-Devices compatibility
// probe in docs/android-companion.md fails. If that happens, the long-poll
// moves in here and the app becomes foreground-only.

var STATUS_DISCONNECTED = 0;
var STATUS_CONNECTED = 1;

Pebble.addEventListener('ready', function () {
  console.log('prh: pkjs ready');

  // TODO (fallback path only): read the server address and device token from
  // localStorage, then start the long-poll loop against GET /v1/poll.
  Pebble.sendAppMessage({ STATUS: STATUS_DISCONNECTED });
});

Pebble.addEventListener('appmessage', function (e) {
  // TODO (fallback path only): forward replies to POST /v1/reply.
  console.log('prh: reply from watch ' + JSON.stringify(e.payload));
});

// Registration lives in the Android companion's settings screen. This config
// page is only wired up in the fallback path, where there is no companion.
//
// TODO (fallback path only): serve a Clay page for host, port, and password,
// call POST /v1/register, and persist the returned token.
Pebble.addEventListener('showConfiguration', function () {
  console.log('prh: configuration is handled by the Android companion');
});
