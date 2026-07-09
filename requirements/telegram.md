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
- Replies beginning with `//` are session input, not Engram commands. Engram
  removes exactly one leading slash before forwarding them to the replied-to
  tracked pane.

## Delivery

- Telegram send/edit failures must be audited.
- `/sessions` must reply even when no Engram sessions exist.
- Empty inline keyboards must not be sent.
- Anchor messages may use Telegram HTML, but fall back to plain text only for
  formatting parse errors. Rate limits and deleted messages must not amplify
  into an immediate second edit.
- Bot API errors are typed and sanitized at the client boundary; request URLs,
  paths, and bot tokens must never appear in returned errors.
- Telegram `retry_after` is honored with bounded, context-aware retry. A
  `message is not modified` edit response counts as success.

## Formatting

- Haiku summaries are not trusted as Telegram-ready markup.
- Engram converts a small Markdown subset to Telegram HTML:
  bold, italic, inline code, and fenced code blocks.
- Conversion must escape raw HTML characters.

## Callbacks

- Refresh, watch, close, and attach callbacks must be bounded to the configured
  user and chat.
- Callback failures must not stop polling.
- Every callback query is answered, including unauthorized, malformed, and
  stale callbacks. Positive text is sent only after validating the target.
- Close buttons open a second confirm/cancel prompt using a random, single-use
  token that expires after two minutes and is invalidated by restart.
