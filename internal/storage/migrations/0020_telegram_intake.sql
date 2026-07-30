-- Telegram intake state.
--
-- telegram_threads maps the Telegram message that carried a loop's HITL ask to
-- that loop. The forward direction (loop -> chat/message) lets the gateway reply
-- into the same thread; the reverse ((chat, message) -> loop) lets an inbound
-- reply route back to the loop that asked. Keyed on the pair because Telegram
-- message ids are only unique within a chat.
CREATE TABLE IF NOT EXISTS telegram_threads (
  chat_id TEXT NOT NULL,
  ask_message_id TEXT NOT NULL,
  loop_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (chat_id, ask_message_id)
);

CREATE INDEX IF NOT EXISTS idx_telegram_threads_loop ON telegram_threads(loop_id);

-- telegram_intake_cursor records the highest Telegram update_id this daemon has
-- finished processing. Telegram's own getUpdates offset is confirm-on-next-call,
-- so a crash between processing and confirming would otherwise replay the batch
-- and create duplicate Issues. A single durable row makes intake idempotent
-- across restarts.
CREATE TABLE IF NOT EXISTS telegram_intake_cursor (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  last_update_id INTEGER NOT NULL,
  updated_at TEXT NOT NULL
);
