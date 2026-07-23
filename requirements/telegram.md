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
- `/remember` lists templates, `/remember <name>` inspects one, and `/remember
  <name> <text>` creates or replaces one. `/forget <name>` removes one.
- `/templates export` uploads one consistent JSON snapshot of the complete
  private template store with the stable Telegram filename `templates.json`.
- Typed terminal input expands explicit `{engram:name}` placeholders once
  immediately before routing. Other brace forms remain literal. Expansion
  applies to ordinary replies and new sessions, escaped slash replies, `/new`,
  `/send`, and `/text`; it never applies to voice-note paths or transcripts.
- `/text` rejects line breaks before tmux delivery, including line breaks
  introduced by a template. It remains a staging command and cannot implicitly
  submit remembered multiline input.
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
- Every inline button label is limited to the seven-rune budget established by
  `🖼️ View`. Dynamic display labels are shortened locally when necessary while
  their callback data remains exact; dense Telegram rows must not rely on the
  client truncating labels.
- Close controls are accepted only from the current canonical message. Close
  uses a random, single-use confirmation token expiring after two minutes; the
  token records the immutable tmux binding and becomes stale after reattachment.
- Lost anchors expose `🧭 Link` for exact-identity recovery and add `♻️ Go`
  only when an allowlisted provider and valid persisted session UUID make a
  native resume exact.
- `/recovery` sends a deterministic, non-model-generated plan for lost
  sessions. It contains copyable `/resume <id>` commands, compact one-tap
  controls for exact provider mappings, advisory observed launches explicitly
  labeled as not replayed, and a dismiss control. Plans are paginated so every
  control remains on the same message as its visible session. Each control is
  bound to the exact watch generation that produced it; older plans are inert.
- Guide anchors expose refresh, the compact non-directional key controls,
  `📄 Raw` for their exact displayed crop, and `🖼️ View` only when Chromium
  is ready. Snapshot anchors additionally expose a distinct `← ↑ ↓ →` row and
  `🗣️ Talk` only when a guide is configured.
- Every running canonical anchor exposes `➖`. It moves that session into one
  shared pinned `Collapsed sessions` shelf, retires and unpins the individual
  anchor, and records the old reply route as stale. The shelf contains bounded
  cached one-line summaries and exactly one `➕` control; replying to it does
  not route input because it represents more than one pane.
- `➕` restores all shelf members. Engram publishes each individual anchor from
  persisted state before queuing ordinary guide or snapshot refreshes. Partial
  restoration leaves the shelf active for the remaining members. After every
  member has a canonical anchor, Engram removes the shelf.
- Collapse is persisted attention state, not a third anchor mode. Collapsed
  sessions perform no model, Chromium, or terminal capture work and expose no
  files, alternate views, or key controls until expanded.
- Directional callbacks are accepted only from the current snapshot anchor, so
  a delayed callback cannot move a terminal after its card returns to guide mode.
- `📄 Raw` uploads the process-local plain-text companion for the most recent
  complete bounded `🖼️ View` frame. In guide mode the canonical card may show a
  compact evidence crop, but Raw still contains the complete View text. It never
  performs a later tmux capture; when restart has cleared that companion, the
  control asks the user to wait for the startup refresh.
- Key callbacks answer immediately before tmux work begins. A later tmux failure
  is delivered as a normal reply rather than leaving Telegram's progress state
  spinning until the terminal timeout.
- When a canonical anchor displays files, one `⬇️ n` button is shown for each
  numbered entry. The callback contains no path: it resolves through the
  current anchor's exact process-local file-list token and then uses the same
  validation, bounded snapshot, queue, and upload path as `/download`.
- `🖼️ View` queues a one-off image reply to a guide anchor. `🗣️ Talk` queues one model
  request over the shared bounded frame's semantic evidence and replies
  conversationally to a snapshot anchor. Neither blocks polling or replaces
  the canonical anchor.
- When guide mode and Chromium are both available, the canonical anchor is a
  single photo card with a compact terminal crop above bounded
  conversational prose. Telegram media edits preserve the canonical message ID, pin,
  controls, and reply route. Missing or unverifiable model evidence falls back
  to a locally computed changed terminal region, then to a bounded physical
  paragraph selected by lexical affinity to the summary with a visible-link
  preference, and finally to the current terminal tail. Model-selected excerpts
  must occur uniquely in both the semantic text
  sent to the provider and the physical capture. Every crop labels its
  provenance without claiming semantic verification. If styled candidates cannot be
  delivered safely, Engram renders the bounded tail as redacted plain text; a
  truly empty terminal receives a quiet guide-only frame. Engram never preserves
  stale pixels, delegates pixel selection to the model, or creates a second message.
- Obsolete media predecessors are deleted when Telegram permits it. If deletion
  fails, Engram replaces their media with a locally generated neutral image,
  clears controls, applies a redacted inactive label, and unpins them. If
  Telegram also refuses media replacement because the message is too old,
  Engram falls back to caption neutralization and audits that historical pixels
  remain outside its control.
- The latest conversational reply and latest screenshot reply for each session
  route Telegram replies to that session. The latest upstream-signal reply has
  the same routing behavior. The canonical guide-evidence card routes through
  the ordinary anchor identity, not an alternate alias. Publishing a newer
  alternate of the same kind makes the predecessor stale. Replies to known stale alternates must
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
  checked again under session-then-anchor delivery locks after the download or
  transcription. Closed targets are rejected before download or provider use;
  asynchronous closure or rotation is rechecked before terminal input. Anchor
  error presentation occurs only after delivery locks are released.
  Successful transcription delivery is followed by a bounded reply containing
  the normalized `(transcribed)` input so recognition errors remain visible.
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
