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
  // Answered elsewhere, e.g. in the VSCode UI. Dismiss it rather than keep
  // asking a settled question.
  EVENT_GONE = 6,
} EventType;

// Mirrors protocol.ReplyAction.
typedef enum {
  REPLY_ONCE = 1,
  REPLY_ALWAYS = 2,
  REPLY_REJECT = 3,
  REPLY_CHOICE = 4,
  REPLY_TEXT = 5,
} ReplyAction;

// Ack status — the companion confirms delivery so the watch can stop
// showing "sending…".
typedef enum {
  ACK_NONE = 0,
  ACK_PENDING,
  ACK_SENT,
  ACK_FAILED,
} AckState;

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
  AckState ack_state;
} s_current;

static Window *s_window;
static TextLayer *s_project_layer;
static TextLayer *s_title_layer;
static TextLayer *s_body_layer;
static TextLayer *s_hint_layer;
static StatusBarLayer *s_status_bar;
static Layer *s_root_layer;
static GRect s_bounds;

#if defined(PBL_MICROPHONE)
static DictationSession *s_dictation;
#endif

// MenuLayer for ques envelopes.
static MenuLayer *s_menu_layer;

// ---------------------------------------------------------------------------
// Forward declarations
// ---------------------------------------------------------------------------

static void render_current(void);

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

// Shows the ack state in the hint layer.
static void update_ack_display(void) {
  switch (s_current.ack_state) {
    case ACK_PENDING:
      text_layer_set_text(s_hint_layer, "Sending…");
      break;
    case ACK_SENT:
      text_layer_set_text(s_hint_layer, "Sent");
      break;
    case ACK_FAILED:
      text_layer_set_text(s_hint_layer, "Failed — press to retry");
      break;
    default:
      render_current();
      break;
  }
}

// Sends a reply for the envelope on screen.
//
// The watch does not retry. If this fails the user sees it — the companion's
// own POST /v1/reply retry is what guarantees delivery, but only once the
// reply reaches the phone.
static void send_reply(ReplyAction action, int choice, const char *text) {
  if (s_current.answered || s_current.id[0] == '\0') {
    return;
  }

  DictionaryIterator *out;
  if (app_message_outbox_begin(&out) != APP_MSG_OK) {
    s_current.ack_state = ACK_FAILED;
    update_ack_display();
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

  if (app_message_outbox_send() == APP_MSG_OK) {
    s_current.answered = true;
    s_current.ack_state = ACK_PENDING;
    update_ack_display();
  } else {
    s_current.ack_state = ACK_FAILED;
    update_ack_display();
  }
}

// Outbox sent callback — the phone received the message.
static void outbox_sent(DictionaryIterator *sent, void *context) {
  if (s_current.ack_state == ACK_PENDING) {
    s_current.ack_state = ACK_SENT;
    update_ack_display();
  }
}

// Outbox failed callback — the phone did not receive the reply.
static void outbox_failed(DictionaryIterator *failed,
                         AppMessageResult reason, void *context) {
  s_current.ack_state = ACK_FAILED;
  update_ack_display();
  APP_LOG(APP_LOG_LEVEL_ERROR, "outbox send failed: %d", (int)reason);
}

// ---------------------------------------------------------------------------
// Dictation
// ---------------------------------------------------------------------------

#if defined(PBL_MICROPHONE)
static void dictation_callback(DictationSession *session, DictationSessionStatus status,
                                char *transcription, void *context) {
  if (status == DictationSessionStatusSuccess) {
    send_reply(REPLY_TEXT, 0, transcription);
  } else {
    text_layer_set_text(s_hint_layer, "Dictation failed");
  }
}
#endif

static void start_dictation(void) {
#if defined(PBL_MICROPHONE)
  if (s_dictation == NULL) {
    s_dictation = dictation_session_create(512, dictation_callback, NULL);
  }
  if (s_dictation != NULL) {
    dictation_session_start(s_dictation);
  }
#endif
}

// ---------------------------------------------------------------------------
// MenuLayer for ques envelopes
// ---------------------------------------------------------------------------

static uint16_t menu_get_num_rows(MenuLayer *menu, uint16_t section, void *context) {
  return (uint16_t)s_current.choice_count;
}

static void menu_draw_row(GContext *ctx, const Layer *cell_layer,
                          MenuIndex *cell_index, void *context) {
  int idx = cell_index->row;
  if (idx < 0 || idx >= s_current.choice_count) return;
  menu_cell_basic_draw(ctx, cell_layer, s_current.choices[idx], NULL, NULL);
}

static int16_t menu_get_cell_height(struct MenuLayer *menu, MenuIndex *cell_index,
                                     void *context) {
  return 36;
}

static void menu_selection_changed(struct MenuLayer *menu, MenuIndex new_index,
                                    MenuIndex old_index, void *context) {
  s_current.selected = new_index.row;
}

static void show_menu(bool show) {
  if (show) {
    if (s_menu_layer == NULL) {
      s_menu_layer = menu_layer_create(
          GRect(0, STATUS_BAR_LAYER_HEIGHT + 20, s_bounds.size.w,
                s_bounds.size.h - STATUS_BAR_LAYER_HEIGHT - 20));
      menu_layer_set_callbacks(s_menu_layer, NULL, (MenuLayerCallbacks){
          .get_num_rows = menu_get_num_rows,
          .draw_row = menu_draw_row,
          .get_cell_height = menu_get_cell_height,
          .selection_changed = menu_selection_changed,
      });
      menu_layer_set_click_config_onto_window(s_menu_layer, s_window);
      layer_add_child(s_root_layer, menu_layer_get_layer(s_menu_layer));
    }
    layer_set_hidden(menu_layer_get_layer(s_menu_layer), false);
    layer_set_hidden(text_layer_get_layer(s_body_layer), true);
    layer_set_hidden(text_layer_get_layer(s_hint_layer), true);
  } else if (s_menu_layer != NULL) {
    layer_set_hidden(menu_layer_get_layer(s_menu_layer), true);
    layer_set_hidden(text_layer_get_layer(s_body_layer), false);
    layer_set_hidden(text_layer_get_layer(s_hint_layer), false);
  }
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// Renders the current envelope. For ques envelopes the body is hidden and a
// MenuLayer shows the choices; for everything else the text layers are used.
static void render_current(void) {
  text_layer_set_text(s_project_layer, s_current.project);
  text_layer_set_text(s_title_layer, s_current.title);
  text_layer_set_text(s_body_layer, s_current.body);

  if (s_current.type == EVENT_QUES && s_current.choice_count > 0) {
    show_menu(true);
    menu_layer_reload_data(s_menu_layer);
    return;
  }
  show_menu(false);

  switch (s_current.type) {
    case EVENT_PERM:
      text_layer_set_text(s_hint_layer, "UP=Yes  MID=Always  DOWN=No");
      break;
    case EVENT_QUES:
      text_layer_set_text(s_hint_layer, "Pick a choice");
      break;
    case EVENT_GONE:
      text_layer_set_text(s_hint_layer, "Answered elsewhere");
      break;
    case EVENT_IDLE:
      text_layer_set_text(s_hint_layer, "Session idle");
      break;
    case EVENT_ERR:
      text_layer_set_text(s_hint_layer, "Session error");
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
static void parse_choices(const char *packed) {
  s_current.choice_count = 0;
  if (packed == NULL || packed[0] == '\0') return;

  const char *start = packed;
  const char *p = packed;
  while (*p != '\0' && s_current.choice_count < MAX_CHOICES) {
    if (*p == CHOICE_SEP) {
      int len = (int)(p - start);
      if (len >= MAX_CHOICE_LEN) len = MAX_CHOICE_LEN - 1;
      strncpy(s_current.choices[s_current.choice_count], start, len);
      s_current.choices[s_current.choice_count][len] = '\0';
      s_current.choice_count++;
      start = p + 1;
    }
    p++;
  }
  // Last segment after the final separator (or the entire string if no sep).
  if (s_current.choice_count < MAX_CHOICES && start < p) {
    int len = (int)(p - start);
    if (len >= MAX_CHOICE_LEN) len = MAX_CHOICE_LEN - 1;
    strncpy(s_current.choices[s_current.choice_count], start, len);
    s_current.choices[s_current.choice_count][len] = '\0';
    s_current.choice_count++;
  }
}

static void inbox_received(DictionaryIterator *iter, void *context) {
  Tuple *id = dict_find(iter, MESSAGE_KEY_EVENT_ID);
  Tuple *type = dict_find(iter, MESSAGE_KEY_EVENT_TYPE);

  // A STATUS-only message: connection state changed, nothing to display.
  if (id == NULL || type == NULL) {
    Tuple *status = dict_find(iter, MESSAGE_KEY_STATUS);
    if (status != NULL) {
      // Could reflect connection state in the status bar. For now just log.
    }
    return;
  }

  // gone: retract the prompt currently on screen.
  if ((EventType)type->value->uint8 == EVENT_GONE) {
    Tuple *id_tuple = dict_find(iter, MESSAGE_KEY_EVENT_ID);
    if (id_tuple != NULL && strcmp(id_tuple->value->cstring, s_current.id) == 0) {
      memset(&s_current, 0, sizeof(s_current));
      show_menu(false);
      text_layer_set_text(s_title_layer, "Dismissed");
      text_layer_set_text(s_body_layer, "");
      text_layer_set_text(s_hint_layer, "");
    }
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
}

// ---------------------------------------------------------------------------
// Buttons
// ---------------------------------------------------------------------------

static void up_click(ClickRecognizerRef recognizer, void *context) {
  // If ack failed, retry.
  if (s_current.ack_state == ACK_FAILED) {
    s_current.answered = false;
    send_reply(REPLY_ONCE, 0, NULL);
    return;
  }
  if (s_current.type == EVENT_QUES) {
    if (s_current.selected > 0) {
      s_current.selected--;
      if (s_menu_layer != NULL) {
        menu_layer_set_selected_index(s_menu_layer, (MenuIndex){.row = s_current.selected},
                                       MenuRowAlignCenter, true);
      }
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
      if (s_menu_layer != NULL) {
        menu_layer_set_selected_index(s_menu_layer, (MenuIndex){.row = s_current.selected},
                                       MenuRowAlignCenter, true);
      }
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
  s_root_layer = window_get_root_layer(window);
  s_bounds = layer_get_bounds(s_root_layer);

  s_status_bar = status_bar_layer_create();
  layer_add_child(s_root_layer, status_bar_layer_get_layer(s_status_bar));

  const int16_t top = STATUS_BAR_LAYER_HEIGHT;

  // The project sits above the action, deliberately: "which repo" has to be
  // legible before "what command" is read.
  s_project_layer = text_layer_create(GRect(4, top, s_bounds.size.w - 8, 20));
  text_layer_set_font(s_project_layer, fonts_get_system_font(FONT_KEY_GOTHIC_14_BOLD));
  text_layer_set_overflow_mode(s_project_layer, GTextOverflowModeTrailingEllipsis);
  layer_add_child(s_root_layer, text_layer_get_layer(s_project_layer));

  s_title_layer = text_layer_create(GRect(4, top + 20, s_bounds.size.w - 8, 32));
  text_layer_set_font(s_title_layer, fonts_get_system_font(FONT_KEY_GOTHIC_24_BOLD));
  text_layer_set_overflow_mode(s_title_layer, GTextOverflowModeTrailingEllipsis);
  layer_add_child(s_root_layer, text_layer_get_layer(s_title_layer));

  s_body_layer = text_layer_create(GRect(4, top + 54, s_bounds.size.w - 8,
      s_bounds.size.h - top - 82));
  text_layer_set_font(s_body_layer, fonts_get_system_font(FONT_KEY_GOTHIC_18));
  text_layer_set_overflow_mode(s_body_layer, GTextOverflowModeWordWrap);
  layer_add_child(s_root_layer, text_layer_get_layer(s_body_layer));

  s_hint_layer = text_layer_create(GRect(4, s_bounds.size.h - 26, s_bounds.size.w - 8, 24));
  text_layer_set_font(s_hint_layer, fonts_get_system_font(FONT_KEY_GOTHIC_14));
  text_layer_set_text_alignment(s_hint_layer, GTextAlignmentCenter);
  layer_add_child(s_root_layer, text_layer_get_layer(s_hint_layer));

  text_layer_set_text(s_title_layer, "Remote Harness");
  text_layer_set_text(s_body_layer, "Waiting for the companion.");
}

static void window_unload(Window *window) {
  if (s_menu_layer != NULL) {
    menu_layer_destroy(s_menu_layer);
    s_menu_layer = NULL;
  }
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
  app_message_register_outbox_sent(outbox_sent);
  app_message_register_outbox_failed(outbox_failed);
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
