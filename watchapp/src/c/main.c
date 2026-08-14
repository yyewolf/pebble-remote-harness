// Pebble Remote Harness — watchapp.
//
// Pure UI. This app never speaks HTTP: the Android companion holds the
// long-poll and pushes envelopes in over AppMessage, launching this app with
// startAppOnPebble() when a prompt arrives. See docs/architecture.md.
//
// Target platform is emery (200x228, 64 colours).

#include <pebble.h>

// Mirrors protocol.EventType.Wire() in api/internal/protocol.
typedef enum {
  EVENT_PERM = 1,
  EVENT_QUES = 2,
  EVENT_IDLE = 3,
  EVENT_ERR = 4,
  EVENT_NOTE = 5,
} EventType;

// Mirrors protocol.ReplyAction.
typedef enum {
  REPLY_ONCE = 1,
  REPLY_ALWAYS = 2,
  REPLY_REJECT = 3,
  REPLY_CHOICE = 4,
  REPLY_TEXT = 5,
} ReplyAction;

#define MAX_PROJECT 25
#define MAX_TITLE 33
#define MAX_BODY 257
#define MAX_ID 33
#define MAX_CHOICES 6
#define MAX_CHOICE_LEN 25
#define CHOICE_SEP '\x1f'

// The envelope currently on screen.
static struct {
  char id[MAX_ID];
  EventType type;
  // Which VSCode window is asking. One prh serves every window, so approving
  // the right command against the wrong repository is a real mistake to make.
  char project[MAX_PROJECT];
  char title[MAX_TITLE];
  char body[MAX_BODY];
  char choices[MAX_CHOICES][MAX_CHOICE_LEN];
  int choice_count;
  int selected;
  bool answered;
} s_current;

static Window *s_window;
static TextLayer *s_project_layer;
static TextLayer *s_title_layer;
static TextLayer *s_body_layer;
static TextLayer *s_hint_layer;
static StatusBarLayer *s_status_bar;

#if defined(PBL_MICROPHONE)
static DictationSession *s_dictation;
#endif

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

// Sends a reply for the envelope on screen.
//
// TODO: the watch does not retry. If this fails the user must see it — the
// companion's own POST /v1/reply retry is what guarantees delivery, but only
// once the reply reaches the phone.
static void send_reply(ReplyAction action, int choice, const char *text) {
  if (s_current.answered || s_current.id[0] == '\0') {
    return;
  }

  DictionaryIterator *out;
  if (app_message_outbox_begin(&out) != APP_MSG_OK) {
    // TODO: surface this. A silently dropped approval is the worst failure
    // mode this app has.
    return;
  }

  dict_write_cstring(out, MESSAGE_KEY_REPLY_ID, s_current.id);
  dict_write_uint8(out, MESSAGE_KEY_REPLY_ACTION, (uint8_t)action);
  if (action == REPLY_CHOICE) {
    dict_write_uint8(out, MESSAGE_KEY_REPLY_CHOICE, (uint8_t)choice);
  }
  if (action == REPLY_TEXT && text != NULL) {
    dict_write_cstring(out, MESSAGE_KEY_REPLY_TEXT, text);
  }

  app_message_outbox_send();
  s_current.answered = true;

  // TODO: show a pending state and only mark answered on the ack, rather
  // than assuming success here.
}

// ---------------------------------------------------------------------------
// Dictation
// ---------------------------------------------------------------------------

#if defined(PBL_MICROPHONE)
static void dictation_callback(DictationSession *session, DictationSessionStatus status,
                               char *transcription, void *context) {
  if (status == DictationSessionStatusSuccess) {
    send_reply(REPLY_TEXT, 0, transcription);
  }
}
#endif

static void start_dictation(void) {
#if defined(PBL_MICROPHONE)
  if (s_dictation == NULL) {
    // TODO: size the buffer against protocol limits rather than guessing.
    s_dictation = dictation_session_create(512, dictation_callback, NULL);
  }
  if (s_dictation != NULL) {
    dictation_session_start(s_dictation);
  }
#endif
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// TODO: implement properly. Currently only the two text layers are filled.
//  - perm: title = action, body = resource, hint = the button legend
//  - ques: render choices as a MenuLayer rather than text, so a long choice
//    list is navigable
//  - idle/err/note: no reply affordances, dismiss on any button
static void render_current(void) {
  text_layer_set_text(s_project_layer, s_current.project);
  text_layer_set_text(s_title_layer, s_current.title);
  text_layer_set_text(s_body_layer, s_current.body);

  switch (s_current.type) {
    case EVENT_PERM:
      text_layer_set_text(s_hint_layer, "Yes / Always / No");
      break;
    case EVENT_QUES:
      text_layer_set_text(s_hint_layer, "Pick a choice");
      break;
    default:
      text_layer_set_text(s_hint_layer, "");
      break;
  }
}

// ---------------------------------------------------------------------------
// Inbound
// ---------------------------------------------------------------------------

// Splits the \x1f-separated CHOICES string into s_current.choices.
//
// TODO: implement.
static void parse_choices(const char *packed) {
  s_current.choice_count = 0;
}

static void inbox_received(DictionaryIterator *iter, void *context) {
  Tuple *id = dict_find(iter, MESSAGE_KEY_EVENT_ID);
  Tuple *type = dict_find(iter, MESSAGE_KEY_EVENT_TYPE);
  if (id == NULL || type == NULL) {
    // A STATUS-only message: connection state changed, nothing to display.
    // TODO: reflect it in the status bar.
    return;
  }

  memset(&s_current, 0, sizeof(s_current));
  strncpy(s_current.id, id->value->cstring, MAX_ID - 1);
  s_current.type = (EventType)type->value->uint8;

  Tuple *project = dict_find(iter, MESSAGE_KEY_PROJECT);
  if (project != NULL) {
    strncpy(s_current.project, project->value->cstring, MAX_PROJECT - 1);
  }
  Tuple *title = dict_find(iter, MESSAGE_KEY_TITLE);
  if (title != NULL) {
    strncpy(s_current.title, title->value->cstring, MAX_TITLE - 1);
  }
  Tuple *body = dict_find(iter, MESSAGE_KEY_BODY);
  if (body != NULL) {
    strncpy(s_current.body, body->value->cstring, MAX_BODY - 1);
  }
  Tuple *choices = dict_find(iter, MESSAGE_KEY_CHOICES);
  if (choices != NULL) {
    parse_choices(choices->value->cstring);
  }

  render_current();

  // A prompt that needs an answer is worth a wrist buzz; a notification is
  // not worth waking someone for.
  if (s_current.type == EVENT_PERM || s_current.type == EVENT_QUES) {
    vibes_double_pulse();
    light_enable_interaction();
  }
}

static void inbox_dropped(AppMessageResult reason, void *context) {
  APP_LOG(APP_LOG_LEVEL_ERROR, "inbox dropped: %d", (int)reason);
  // TODO: ask the companion to resend. A dropped prompt currently vanishes.
}

// ---------------------------------------------------------------------------
// Buttons
// ---------------------------------------------------------------------------

static void up_click(ClickRecognizerRef recognizer, void *context) {
  if (s_current.type == EVENT_QUES) {
    if (s_current.selected > 0) {
      s_current.selected--;
      render_current();
    }
    return;
  }
  send_reply(REPLY_ONCE, 0, NULL);
}

static void select_click(ClickRecognizerRef recognizer, void *context) {
  if (s_current.type == EVENT_QUES) {
    send_reply(REPLY_CHOICE, s_current.selected, NULL);
    return;
  }
  send_reply(REPLY_ALWAYS, 0, NULL);
}

static void select_long_click(ClickRecognizerRef recognizer, void *context) {
  start_dictation();
}

static void down_click(ClickRecognizerRef recognizer, void *context) {
  if (s_current.type == EVENT_QUES) {
    if (s_current.selected < s_current.choice_count - 1) {
      s_current.selected++;
      render_current();
    }
    return;
  }
  send_reply(REPLY_REJECT, 0, NULL);
}

static void click_config(void *context) {
  window_single_click_subscribe(BUTTON_ID_UP, up_click);
  window_single_click_subscribe(BUTTON_ID_SELECT, select_click);
  window_single_click_subscribe(BUTTON_ID_DOWN, down_click);
  window_long_click_subscribe(BUTTON_ID_SELECT, 0, select_long_click, NULL);
  // BACK keeps its default: leave, without answering. The prompt stays
  // pending in prh until it expires or is answered elsewhere.
}

// ---------------------------------------------------------------------------
// Window
// ---------------------------------------------------------------------------

static void window_load(Window *window) {
  Layer *root = window_get_root_layer(window);
  GRect bounds = layer_get_bounds(root);

  s_status_bar = status_bar_layer_create();
  layer_add_child(root, status_bar_layer_get_layer(s_status_bar));

  const int16_t top = STATUS_BAR_LAYER_HEIGHT;

  // The project sits above the action, deliberately: "which repo" has to be
  // legible before "what command" is read.
  s_project_layer = text_layer_create(GRect(4, top, bounds.size.w - 8, 20));
  text_layer_set_font(s_project_layer, fonts_get_system_font(FONT_KEY_GOTHIC_14_BOLD));
  text_layer_set_overflow_mode(s_project_layer, GTextOverflowModeTrailingEllipsis);
  layer_add_child(root, text_layer_get_layer(s_project_layer));

  s_title_layer = text_layer_create(GRect(4, top + 20, bounds.size.w - 8, 32));
  text_layer_set_font(s_title_layer, fonts_get_system_font(FONT_KEY_GOTHIC_24_BOLD));
  text_layer_set_overflow_mode(s_title_layer, GTextOverflowModeTrailingEllipsis);
  layer_add_child(root, text_layer_get_layer(s_title_layer));

  s_body_layer = text_layer_create(GRect(4, top + 54, bounds.size.w - 8, bounds.size.h - top - 82));
  text_layer_set_font(s_body_layer, fonts_get_system_font(FONT_KEY_GOTHIC_18));
  text_layer_set_overflow_mode(s_body_layer, GTextOverflowModeWordWrap);
  layer_add_child(root, text_layer_get_layer(s_body_layer));

  s_hint_layer = text_layer_create(GRect(4, bounds.size.h - 26, bounds.size.w - 8, 24));
  text_layer_set_font(s_hint_layer, fonts_get_system_font(FONT_KEY_GOTHIC_14));
  text_layer_set_text_alignment(s_hint_layer, GTextAlignmentCenter);
  layer_add_child(root, text_layer_get_layer(s_hint_layer));

  text_layer_set_text(s_title_layer, "Remote Harness");
  text_layer_set_text(s_body_layer, "Waiting for the companion.");
}

static void window_unload(Window *window) {
  text_layer_destroy(s_project_layer);
  text_layer_destroy(s_title_layer);
  text_layer_destroy(s_body_layer);
  text_layer_destroy(s_hint_layer);
  status_bar_layer_destroy(s_status_bar);
}

static void init(void) {
  s_window = window_create();
  window_set_click_config_provider(s_window, click_config);
  window_set_window_handlers(s_window, (WindowHandlers){
                                           .load = window_load,
                                           .unload = window_unload,
                                       });

  app_message_register_inbox_received(inbox_received);
  app_message_register_inbox_dropped(inbox_dropped);
  // TODO: size these from the real envelope limits in docs/protocol.md and
  // handle a smaller negotiated buffer gracefully.
  app_message_open(app_message_inbox_size_maximum(), app_message_outbox_size_maximum());

  window_stack_push(s_window, true);
}

static void deinit(void) {
#if defined(PBL_MICROPHONE)
  if (s_dictation != NULL) {
    dictation_session_destroy(s_dictation);
  }
#endif
  window_destroy(s_window);
}

int main(void) {
  init();
  app_event_loop();
  deinit();
  return 0;
}
