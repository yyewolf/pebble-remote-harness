// Pebble Remote Harness — watchapp.
//
// Pure UI. This app never speaks HTTP: the Android companion holds the
// long-poll and pushes envelopes in over AppMessage, launching this app with
// startAppOnPebble() when a prompt arrives. See docs/architecture.md.
//
// Target platform is emery (200x228, 64 colours).

#include <pebble.h>
#include <stdio.h>

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

// Bounded queue of pending prompts. Eight simultaneous outstanding
// permissions is well beyond any real session; beyond this the oldest is
// dropped (it stays pending in prh and can be answered at the desk).
#define MAX_QUEUE 8

// One entry in the prompt queue. Mirrors the old s_current struct, plus
// the last action/choice so a failed send can be retried.
typedef struct {
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
  ReplyAction last_action;
  int last_choice;
} PromptEntry;

static PromptEntry s_queue[MAX_QUEUE];
static int s_queue_count;
static int s_queue_pos;

// Transient (non-queued) display for idle/err notifications. Only shown when
// the queue is empty; a pending prompt always takes priority.
static struct {
  EventType type;
  char project[MAX_PROJECT];
  char body[MAX_BODY];
  bool valid;
} s_transient;

static Window *s_window;
static TextLayer *s_project_layer;
static TextLayer *s_queue_indicator_layer;
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
// Queue operations
// ---------------------------------------------------------------------------

// Returns the currently-displayed queue entry, or NULL if the queue is empty.
static PromptEntry *current_entry(void) {
  if (s_queue_count == 0) return NULL;
  if (s_queue_pos < 0 || s_queue_pos >= s_queue_count) s_queue_pos = 0;
  return &s_queue[s_queue_pos];
}

// Removes the entry at idx, shifting the rest down. Adjusts s_queue_pos so
// it still points at a valid entry (or 0 if the queue becomes empty).
static void queue_remove_at(int idx) {
  if (idx < 0 || idx >= s_queue_count) return;
  for (int i = idx; i < s_queue_count - 1; i++) {
    s_queue[i] = s_queue[i + 1];
  }
  s_queue_count--;
  if (s_queue_count == 0) {
    s_queue_pos = 0;
  } else if (s_queue_pos > idx) {
    s_queue_pos--;
  } else if (s_queue_pos >= s_queue_count) {
    s_queue_pos = s_queue_count - 1;
  }
}

// Finds and removes the entry whose id matches. No-op if not found.
static void queue_remove_by_id(const char *id) {
  for (int i = 0; i < s_queue_count; i++) {
    if (strcmp(s_queue[i].id, id) == 0) {
      queue_remove_at(i);
      return;
    }
  }
}

// Sets the ack state of the entry whose id matches.
static void queue_set_ack_by_id(const char *id, AckState state) {
  for (int i = 0; i < s_queue_count; i++) {
    if (strcmp(s_queue[i].id, id) == 0) {
      s_queue[i].ack_state = state;
      return;
    }
  }
}

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

// Sends a reply for the envelope currently on screen.
//
// The watch does not retry. If this fails the user sees it — the companion's
// own POST /v1/reply retry is what guarantees delivery, but only once the
// reply reaches the phone.
static void send_reply(ReplyAction action, int choice, const char *text) {
  PromptEntry *e = current_entry();
  if (e == NULL || e->answered || e->id[0] == '\0') {
    return;
  }

  DictionaryIterator *out;
  if (app_message_outbox_begin(&out) != APP_MSG_OK) {
    e->ack_state = ACK_FAILED;
    render_current();
    return;
  }

  dict_write_cstring(out, MESSAGE_KEY_REPLY_ID, e->id);
  dict_write_uint8(out, MESSAGE_KEY_REPLY_ACTION, (uint8_t)action);
  if (action == REPLY_CHOICE) {
    dict_write_uint8(out, MESSAGE_KEY_REPLY_CHOICE, (uint8_t)choice);
  }
  if (action == REPLY_TEXT && text != NULL) {
    dict_write_cstring(out, MESSAGE_KEY_REPLY_TEXT, text);
  }

  if (app_message_outbox_send() == APP_MSG_OK) {
    e->answered = true;
    e->ack_state = ACK_PENDING;
    e->last_action = action;
    e->last_choice = choice;
    render_current();
  } else {
    e->ack_state = ACK_FAILED;
    render_current();
  }
}

// Retries the last reply for the current entry after a failed send.
static void retry_send(void) {
  PromptEntry *e = current_entry();
  if (e == NULL || e->ack_state != ACK_FAILED) return;
  e->answered = false;
  e->ack_state = ACK_NONE;
  send_reply(e->last_action, e->last_choice, NULL);
}

// Outbox sent callback — the phone received the message. Remove the answered
// prompt from the queue and advance to the next.
//
// The REPLY_ID in the sent dictionary identifies which prompt was acked, so
// the correct entry is removed even if the user has since cycled to another.
static void outbox_sent(DictionaryIterator *sent, void *context) {
  Tuple *t = dict_find(sent, MESSAGE_KEY_REPLY_ID);
  if (t != NULL) {
    queue_remove_by_id(t->value->cstring);
  }
  render_current();
}

// Outbox failed callback — the phone did not receive the reply.
static void outbox_failed(DictionaryIterator *failed,
                         AppMessageResult reason, void *context) {
  Tuple *t = dict_find(failed, MESSAGE_KEY_REPLY_ID);
  if (t != NULL) {
    queue_set_ack_by_id(t->value->cstring, ACK_FAILED);
  }
  render_current();
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
  PromptEntry *e = current_entry();
  return e ? (uint16_t)e->choice_count : 0;
}

static void menu_draw_row(GContext *ctx, const Layer *cell_layer,
                          MenuIndex *cell_index, void *context) {
  PromptEntry *e = current_entry();
  if (e == NULL) return;
  int idx = cell_index->row;
  if (idx < 0 || idx >= e->choice_count) return;
  menu_cell_basic_draw(ctx, cell_layer, e->choices[idx], NULL, NULL);
}

static int16_t menu_get_cell_height(struct MenuLayer *menu, MenuIndex *cell_index,
                                     void *context) {
  return 36;
}

static void menu_selection_changed(struct MenuLayer *menu, MenuIndex new_index,
                                    MenuIndex old_index, void *context) {
  PromptEntry *e = current_entry();
  if (e != NULL) {
    e->selected = new_index.row;
  }
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

// Updates the queue position indicator ("1/3"). Hidden when there is fewer
// than two pending prompts.
static void update_queue_indicator(void) {
  if (s_queue_count > 1) {
    static char buf[24];
    snprintf(buf, sizeof(buf), "%d/%d", s_queue_pos + 1, s_queue_count);
    text_layer_set_text(s_queue_indicator_layer, buf);
  } else {
    text_layer_set_text(s_queue_indicator_layer, "");
  }
}

// Renders the current state: the queue entry at s_queue_pos, or the transient
// idle/err notification, or the idle "waiting" screen.
static void render_current(void) {
  update_queue_indicator();

  PromptEntry *e = current_entry();

  if (e == NULL) {
    // Queue empty — show transient notification or the waiting screen.
    show_menu(false);
    if (s_transient.valid) {
      text_layer_set_text(s_project_layer, s_transient.project);
      text_layer_set_text(s_title_layer,
          s_transient.type == EVENT_IDLE ? "idle" : "error");
      text_layer_set_text(s_body_layer, s_transient.body);
      text_layer_set_text(s_hint_layer,
          s_transient.type == EVENT_IDLE ? "Session idle" : "Session error");
    } else {
      text_layer_set_text(s_project_layer, "");
      text_layer_set_text(s_title_layer, "Remote Harness");
      text_layer_set_text(s_body_layer, "Waiting for the companion.");
      text_layer_set_text(s_hint_layer, "");
    }
    return;
  }

  // A prompt is on screen. The project (workspace basename) sits above the
  // action — "which repo" must be legible before "what command" is read.
  text_layer_set_text(s_project_layer, e->project);
  text_layer_set_text(s_title_layer, e->title);
  text_layer_set_text(s_body_layer, e->body);

  // Menu for ques envelopes with choices.
  if (e->type == EVENT_QUES && e->choice_count > 0) {
    show_menu(true);
    menu_layer_reload_data(s_menu_layer);
  } else {
    show_menu(false);
  }

  // Ack state overrides the hint when a reply is in flight.
  switch (e->ack_state) {
    case ACK_PENDING:
      text_layer_set_text(s_hint_layer, "Sending…");
      return;
    case ACK_SENT:
      text_layer_set_text(s_hint_layer, "Sent");
      return;
    case ACK_FAILED:
      text_layer_set_text(s_hint_layer, "Failed — press to retry");
      return;
    default:
      break;
  }

  switch (e->type) {
    case EVENT_PERM:
      text_layer_set_text(s_hint_layer, "UP=Yes  MID=Always  DOWN=No");
      break;
    case EVENT_QUES:
      text_layer_set_text(s_hint_layer, "Pick a choice");
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

// Splits the \x1f-separated CHOICES string into the current entry's choices.
static void parse_choices(PromptEntry *e, const char *packed) {
  e->choice_count = 0;
  if (packed == NULL || packed[0] == '\0') return;

  const char *start = packed;
  const char *p = packed;
  while (*p != '\0' && e->choice_count < MAX_CHOICES) {
    if (*p == CHOICE_SEP) {
      int len = (int)(p - start);
      if (len >= MAX_CHOICE_LEN) len = MAX_CHOICE_LEN - 1;
      strncpy(e->choices[e->choice_count], start, len);
      e->choices[e->choice_count][len] = '\0';
      e->choice_count++;
      start = p + 1;
    }
    p++;
  }
  // Last segment after the final separator (or the entire string if no sep).
  if (e->choice_count < MAX_CHOICES && start < p) {
    int len = (int)(p - start);
    if (len >= MAX_CHOICE_LEN) len = MAX_CHOICE_LEN - 1;
    strncpy(e->choices[e->choice_count], start, len);
    e->choices[e->choice_count][len] = '\0';
    e->choice_count++;
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

  EventType ev_type = (EventType)type->value->uint8;

  // gone: retract the prompt from the queue, wherever it is.
  if (ev_type == EVENT_GONE) {
    Tuple *id_tuple = dict_find(iter, MESSAGE_KEY_EVENT_ID);
    if (id_tuple != NULL) {
      queue_remove_by_id(id_tuple->value->cstring);
    }
    render_current();
    return;
  }

  // idle and err are notify-only. Show them only when no prompt is pending;
  // a pending prompt is more urgent and must not be displaced.
  if (ev_type == EVENT_IDLE || ev_type == EVENT_ERR) {
    if (s_queue_count > 0) return;

    memset(&s_transient, 0, sizeof(s_transient));
    s_transient.valid = true;
    s_transient.type = ev_type;

    Tuple *project = dict_find(iter, MESSAGE_KEY_PROJECT);
    if (project != NULL) {
      strncpy(s_transient.project, project->value->cstring, MAX_PROJECT - 1);
    }
    Tuple *body = dict_find(iter, MESSAGE_KEY_BODY);
    if (body != NULL) {
      strncpy(s_transient.body, body->value->cstring, MAX_BODY - 1);
    }

    render_current();
    return;
  }

  // perm and ques need a reply — enqueue them.
  if (s_queue_count >= MAX_QUEUE) {
    // Drop the oldest; it stays pending in prh and can be answered at the
    // desk. The newest prompt is the one the user was just woken for.
    queue_remove_at(0);
  }

  int idx = s_queue_count;
  PromptEntry *e = &s_queue[idx];
  memset(e, 0, sizeof(PromptEntry));

  strncpy(e->id, id->value->cstring, MAX_ID - 1);
  e->type = ev_type;
  e->ack_state = ACK_NONE;

  Tuple *project = dict_find(iter, MESSAGE_KEY_PROJECT);
  if (project != NULL) {
    strncpy(e->project, project->value->cstring, MAX_PROJECT - 1);
  }
  Tuple *title = dict_find(iter, MESSAGE_KEY_TITLE);
  if (title != NULL) {
    strncpy(e->title, title->value->cstring, MAX_TITLE - 1);
  }
  Tuple *body = dict_find(iter, MESSAGE_KEY_BODY);
  if (body != NULL) {
    strncpy(e->body, body->value->cstring, MAX_BODY - 1);
  }
  Tuple *choices = dict_find(iter, MESSAGE_KEY_CHOICES);
  if (choices != NULL) {
    parse_choices(e, choices->value->cstring);
  }

  s_queue_count++;
  // Jump to the new prompt — the user was woken for this one.
  s_queue_pos = idx;

  // Clear any stale transient so it does not reappear when the queue empties.
  s_transient.valid = false;

  render_current();

  // A prompt that needs an answer is worth a wrist buzz; a notification is
  // not worth waking someone for.
  vibes_double_pulse();
  light_enable_interaction();
}

static void inbox_dropped(AppMessageResult reason, void *context) {
  APP_LOG(APP_LOG_LEVEL_ERROR, "inbox dropped: %d", (int)reason);
}

// ---------------------------------------------------------------------------
// Buttons
// ---------------------------------------------------------------------------

static void up_click(ClickRecognizerRef recognizer, void *context) {
  PromptEntry *e = current_entry();

  // If the current prompt's send failed, retry.
  if (e != NULL && e->ack_state == ACK_FAILED) {
    retry_send();
    return;
  }
  if (e != NULL && e->type == EVENT_QUES) {
    if (e->selected > 0) {
      e->selected--;
      if (s_menu_layer != NULL) {
        menu_layer_set_selected_index(s_menu_layer, (MenuIndex){.row = e->selected},
                                       MenuRowAlignCenter, true);
      }
    }
    return;
  }
  send_reply(REPLY_ONCE, 0, NULL);
}

static void select_click(ClickRecognizerRef recognizer, void *context) {
  PromptEntry *e = current_entry();
  if (e == NULL) return;

  if (e->type == EVENT_QUES) {
    send_reply(REPLY_CHOICE, e->selected, NULL);
    return;
  }
  send_reply(REPLY_ALWAYS, 0, NULL);
}

static void select_long_click(ClickRecognizerRef recognizer, void *context) {
  start_dictation();
}

static void down_click(ClickRecognizerRef recognizer, void *context) {
  PromptEntry *e = current_entry();

  if (e != NULL && e->ack_state == ACK_FAILED) {
    retry_send();
    return;
  }
  if (e != NULL && e->type == EVENT_QUES) {
    if (e->selected < e->choice_count - 1) {
      e->selected++;
      if (s_menu_layer != NULL) {
        menu_layer_set_selected_index(s_menu_layer, (MenuIndex){.row = e->selected},
                                       MenuRowAlignCenter, true);
      }
    }
    return;
  }
  send_reply(REPLY_REJECT, 0, NULL);
}

// BACK cycles through pending prompts when there is more than one. With a
// single (or no) prompt, it exits the app as usual — the prompt stays pending
// in prh until it expires or is answered elsewhere.
static void back_click(ClickRecognizerRef recognizer, void *context) {
  if (s_queue_count > 1) {
    s_queue_pos = (s_queue_pos + 1) % s_queue_count;
    render_current();
  } else {
    window_stack_pop(true);
  }
}

static void click_config(void *context) {
  window_single_click_subscribe(BUTTON_ID_UP, up_click);
  window_single_click_subscribe(BUTTON_ID_SELECT, select_click);
  window_single_click_subscribe(BUTTON_ID_DOWN, down_click);
  window_long_click_subscribe(BUTTON_ID_SELECT, 0, select_long_click, NULL);
  window_single_click_subscribe(BUTTON_ID_BACK, back_click);
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

  // The project (workspace basename) sits above the action, deliberately:
  // "which repo" has to be legible before "what command" is read. The queue
  // indicator ("1/3") sits to its right.
  s_project_layer = text_layer_create(GRect(4, top, s_bounds.size.w - 48, 20));
  text_layer_set_font(s_project_layer, fonts_get_system_font(FONT_KEY_GOTHIC_14_BOLD));
  text_layer_set_overflow_mode(s_project_layer, GTextOverflowModeTrailingEllipsis);
  layer_add_child(s_root_layer, text_layer_get_layer(s_project_layer));

  // Queue position indicator, right-aligned in the same row as the project.
  s_queue_indicator_layer = text_layer_create(GRect(s_bounds.size.w - 44, top, 40, 20));
  text_layer_set_font(s_queue_indicator_layer, fonts_get_system_font(FONT_KEY_GOTHIC_14_BOLD));
  text_layer_set_text_alignment(s_queue_indicator_layer, GTextAlignmentRight);
  layer_add_child(s_root_layer, text_layer_get_layer(s_queue_indicator_layer));

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
  text_layer_destroy(s_queue_indicator_layer);
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
