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
- Every public slash command handled by the app must have metadata. The parser
  may temporarily accept undocumented compatibility aliases for renamed input
  operations, but aliases must not appear in help, registration, or command
  JSON.
- Telegram-invalid command names must not be registered with Telegram.
- Replies beginning with `//` are session input, not Engram commands. Engram
  removes exactly one leading slash before forwarding them to the replied-to
  tracked pane.

## Delivery

- Telegram send/edit failures must be audited.
- `/sessions` must reply even when no Engram sessions exist.
- `/sessions` groups tracked work as lost, needs attention, worth reviewing,
  and working quietly. Attention groups use Haiku's latest assessment; lost is
  deterministic and takes precedence. Sessions within a group use the newest
  assessment transition first.
- Empty inline keyboards must not be sent.
- Anchor messages may use Telegram HTML, but fall back to plain text only for
  formatting parse errors. Rate limits and deleted messages must not amplify
  into an immediate second edit.
- A failed initial anchor delivery leaves the session unwatched. Replacement
  anchors are created only when Telegram reports the original missing, too old,
  or otherwise uneditable; transient server errors retain the original anchor.
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

- Refresh, reattach, watch, close, and attach callbacks must be bounded to the
  configured user and chat.
- Callback failures must not stop polling.
- Every callback query is answered, including unauthorized, malformed, and
  stale callbacks. Positive text is sent only after validating the target.
- Close buttons open a second confirm/cancel prompt using a random, single-use
  token that expires after two minutes and is invalidated by restart.
- Closed and lost anchors expose no key or refresh controls.
- A lost anchor exposes only `🧭 Reattach`. It restores the session when its
  original immutable pane/window identity is live and otherwise directs the
  user to `/sessions`.

## Attention

- Attention assessment initially changes anchor text and `/sessions` ordering
  only. Output changes and attention transitions must not create new Telegram
  notification messages.
- An `act` anchor may show a quiet `needs you` line. The assessment remains
  subordinate to deterministic state and cited terminal evidence.
