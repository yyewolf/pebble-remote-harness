// PebbleKit JS — reply relay.
//
// The Core Devices app routes inbound AppMessages to PKJS, not to classic
// PebbleKit broadcast receivers. So PKJS is the only path that receives
// watch replies. It forwards them to the companion, which forwards to prh.
//
// The companion still holds the long-poll and wakes the watchapp — PKJS
// only handles the reply direction.

var STATUS_DISCONNECTED = 0;
var STATUS_CONNECTED = 1;

var COMPANION_URL = "http://127.0.0.1:8478";

Pebble.addEventListener('ready', function () {
  console.log('prh: pkjs ready');
  // Deliberately sends nothing on ready — the companion handles everything.
});

Pebble.addEventListener('appmessage', function (e) {
  console.log('prh: reply from watch ' + JSON.stringify(e.payload));

  var replyId = e.payload.REPLY_ID;
  var replyAction = e.payload.REPLY_ACTION;
  if (!replyId || replyAction === undefined) return;

  var url = COMPANION_URL;
  if (!url) {
    console.log('prh: no companion URL set, dropping reply');
    return;
  }

  // Forward the reply to the companion's local HTTP relay.
  var body = JSON.stringify({
    event_id: replyId,
    action: actionSlug(replyAction),
    choice: e.payload.REPLY_CHOICE || 0,
    text: e.payload.REPLY_TEXT || ""
  });

  console.log('prh: forwarding reply to ' + url + ' body=' + body);

  try {
    var req = new XMLHttpRequest();
    req.open("POST", url + "/pkjs/reply", false);
    req.setRequestHeader("Content-Type", "application/json");
    req.send(body);
    console.log('prh: reply forwarded, status=' + req.status);
  } catch (err) {
    console.log('prh: reply forward failed: ' + err);
  }
});

function actionSlug(wire) {
  switch (wire) {
    case 1: return "once";
    case 2: return "always";
    case 3: return "reject";
    case 4: return "choice";
    case 5: return "text";
    default: return "once";
  }
}

Pebble.addEventListener('showConfiguration', function () {
  console.log('prh: configuration is handled by the Android companion');
});
