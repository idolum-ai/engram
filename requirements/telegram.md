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
- `/sessions` groups tracked work as lost, needs you, and quiet. Lost is
  deterministic and takes precedence. Unacknowledged handoffs appear oldest
  first with a compact recommended action; acknowledged handoffs remain quiet
  with an observing marker.
- Empty inline keyboards must not be attached to newly sent messages. An edit
  may include an explicitly empty keyboard to retire an anchor's controls.
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
- Refresh, key, and reattach callbacks must come from the session's current
  canonical anchor. Controls on a retired or superseded message are inert even
  when Telegram has not yet removed their visible keyboard.
- Close buttons open a second confirm/cancel prompt using a random, single-use
  token that expires after two minutes and is invalidated by restart.
- Closed and lost anchors expose no key or refresh controls.
- A lost anchor exposes only `🧭 Reattach`. It restores the session when its
  original immutable pane/window identity is live and otherwise directs the
  user to `/sessions`.

## Handoffs

- Opening, replacing, or reopening a settled handoff rotates the session's live
  anchor. Engram sends the full new anchor as a reply to its predecessor, makes
  the new message canonical, and continues all later rendering there.
- After canonical state is durable, Engram compacts the predecessor to a short
  continuation marker, clears its keyboard, and unpins it. A transient edit or
  unpin failure remains persisted for retry; routing and callbacks still accept
  only the canonical message.
- A failed new-anchor send leaves the predecessor canonical. A state failure
  after sending the prospective anchor causes a best-effort removal of its
  controls and pin. A crash in that external-action window may leave an inert
  orphan message, but it must not create two routable anchors.
- Repeated captures of the same open handoff do not rotate again. An
  unacknowledged handoff adds `needs you` to the live anchor. After input, that
  same anchor says Engram is observing until later evidence resolves or reopens
  the handoff.

## Pinned Anchors

- Every running watched session with an anchor should have its canonical anchor
  silently pinned in the configured DM. Pinning must not create a notification.
- Rotation pins the new canonical anchor before unpinning its predecessor.
- Unwatch, close, and deterministic identity loss unpin the current anchor.
  Rewatch or exact-identity recovery pins it again.
- Pin state is unknown after process start and must be reconciled once for every
  tracked anchor. Pin and unpin failures are audited and retried without
  blocking tmux input or anchor rendering.
