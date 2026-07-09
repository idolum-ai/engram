# Telegram Requirements

Telegram is Engram's only user interface.

## Admission

- Engram accepts exactly one configured Telegram user.
- For direct messages, `TELEGRAM_CHAT_ID` may be omitted and defaults to
  `TELEGRAM_ALLOWED_USER_ID`.
- Unauthorized messages must not mutate tmux or state.

## Commands

- Command metadata lives in `internal/commands`.
- `/help`, Telegram bot command registration, and `engram commands` must derive
  from the same registry.
- Every slash command handled by the app must have metadata.
- Telegram-invalid command names must not be registered with Telegram.

## Delivery

- Telegram send/edit failures must be audited.
- `/sessions` must reply even when no Engram sessions exist.
- Empty inline keyboards must not be sent.
- Anchor messages may use Telegram HTML, but must fall back to plain text when
  Telegram rejects formatting.

## Formatting

- Haiku summaries are not trusted as Telegram-ready markup.
- Engram converts a small Markdown subset to Telegram HTML:
  bold, italic, inline code, and fenced code blocks.
- Conversion must escape raw HTML characters.

## Callbacks

- Refresh, watch, close, and attach callbacks must be bounded to the configured
  user and chat.
- Callback failures must not stop polling.
