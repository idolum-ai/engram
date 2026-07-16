# Telegram Requirements

## API Endpoint

- The Telegram Bot API server root defaults to `https://api.telegram.org` and
  may be replaced with `TELEGRAM_API_BASE` at process startup.
- Method calls use `<base>/bot<token>/<method>` and file downloads use
  `<base>/file/bot<token>/<path>` after removing trailing slashes from the base.
- The base must be an absolute HTTP(S) URL without userinfo, query, or fragment.
- Endpoint changes require a service restart; they are not a Telegram command.

Telegram is Engram's only user interface.

## Admission And Commands

- Engram accepts exactly one configured user and one private chat.
- Unauthorized updates must not reach tmux or application handlers. Bounded
  admission bookkeeping may advance without retaining rejected identifiers.
- Command metadata has one source: `/help`, Bot API registration, and
  `engram commands` derive from the registry. Every public slash command handled
  by the app has metadata.
- Replies beginning `//` are escaped pane input, not Engram commands.
- `/mode` reports the current and available presentations, distinguishing the
  configured guide provider from locally probed Chromium. `/mode guide` or `/mode
  snapshot` begins migration only when the target capability is available, and
  the selected mode persists across restart.

## Delivery

- `/sessions` always replies and groups tracked work as lost, then active by
  recency. Presentation mode and model output do not alter this ordering.
- Telegram send/edit failures are audited. Empty keyboards are not attached to
  new messages; explicit empty keyboards may retire controls.
- Anchor HTML falls back to plain text only for formatting errors. Rate limits
  and unchanged edits must not amplify into replacement messages.
- Initial delivery failure leaves a session unwatched. A replacement anchor is
  created only when Telegram says the canonical message is uneditable.
- Bot API errors are typed and sanitized; `retry_after` is honored with bounded,
  context-aware retry.
- Model prose is not trusted as Telegram markup. Engram escapes raw HTML and
  converts only its supported Markdown subset.

## Callbacks And Alternate Views

- Every callback is answered and authorized against configured user, chat, and
  current canonical message. Retired controls are inert.
- Close controls are accepted only from the current canonical message. Close
  uses a random, single-use confirmation token expiring after two minutes; the
  token records the immutable tmux binding and becomes stale after reattachment.
- Lost anchors expose only `🧭 Reattach` for exact-identity recovery.
- Guide anchors expose refresh, allowed keys, and `🖼️` only when Chromium is
  ready. Snapshot anchors expose refresh, allowed keys, and `🗣️` only when
  a guide is configured.
- `🖼️` queues a one-off image reply to a guide anchor. `🗣️` queues one model
  request over the shared bounded frame's semantic evidence and replies
  conversationally to a snapshot anchor. Neither blocks polling or replaces
  the canonical anchor.
- The latest conversational reply and latest screenshot reply for each session
  route Telegram replies to that session. The latest upstream-signal reply has
  the same routing behavior. Publishing a newer alternate of the same kind
  makes the predecessor stale. Replies to known stale alternates must
  not reach tmux and receive a concise normal bot reply; Telegram offers no
  callback-style ephemeral banner for an ordinary message reply.
- A Telegram voice note replying to any current routable message follows the
  same latest-only rule. `VOICE_INPUT_MODE=path`, the default, downloads it
  through the bounded transfer queue, retains it in the private attachment
  store, and sends one `(voice message: <absolute-path>)` guarded paste plus
  Enter. `VOICE_INPUT_MODE=transcribe` instead requires `OPENAI_API_KEY`, sends
  a temporary copy once to the admitted non-streaming model, normalizes the
  result to one bounded line, prefixes `(transcribed)`, and sends one guarded
  paste plus Enter. The current reply identity and immutable tmux binding are
  checked again under their delivery locks after the download or transcription.
  A stale, unknown, oversized, unsafe, failed, or identity-changed voice reply
  sends no terminal input. A transcription failure does not fall back to a path.
- Voice notes that are not replies retain ordinary attachment behavior. Voice
  mode is independent of `LLM_PROVIDER`, is selected only at startup, and never
  becomes transcription merely because an OpenAI credential exists.
- Alternate delivery is committed only while the complete tmux binding, mode,
  and canonical anchor still match. A prospective alternate that loses this
  race or cannot persist its reply alias is deleted; an uncertain post-replace
  persistence result is audited rather than contradicted by deletion.
- Snapshot anchors edit their media in place for changed frames. Mode migration
  preserves one canonical, routable anchor even where Telegram requires
  retiring a predecessor.

## Pinned Anchors

- Every running watched session should have its canonical anchor silently
  pinned. Unwatch, close, and identity loss unpin it.
- Pin state is reconciled after restart. Pin failures are audited and retried
  without blocking tmux input or rendering.
