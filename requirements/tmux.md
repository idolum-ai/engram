# tmux Requirements

tmux is the source of terminal truth.

## Session Selection

- If `ENGRAM_TMUX_SESSION` is configured, Engram uses it and creates it if
  missing. Otherwise Engram uses the first existing session, or creates
  `engram-<chat-id>` when no session exists.

## Windows And Attachments

- A top-level non-command Telegram message creates a new tmux window.
- `/attach <target>` tracks an existing target's active pane.
- `/sessions` shows native tmux sessions and immutable pane identities with
  attach buttons for untracked panes.
- Attach callbacks carry `%pane_id`, not mutable indexes.
- Before input, capture, cwd lookup, or destructive close, Engram verifies that
  `%pane_id` still belongs to the stored `@window_id`. A mismatch marks the
  session lost. Transient command failure does not.
- A lost anchor can reattach only when that exact pane/window identity returns;
  Engram must not guess a replacement.

## Input

- Replies to the canonical anchor send literal text and Enter to its pane.
  The latest conversational and screenshot replies are aliases to the same
  pane. Retired anchors and stale alternate replies do not route input.
- A reply beginning `//` removes one slash and sends the resulting slash-led
  input downstream. `/text` omits Enter; `/key` sends validated tmux key names.
- Live anchors include `Esc`, `Escx2`, `^C`, `^D`, and `Enter`. `Escx2` waits
  500 ms between Escape keys.

## Capture And Presentation

- Both anchor modes use the same ANSI-preserving `CaptureStyled` result. It
  targets and caps at 64 rows ending at the pane bottom, using available recent
  scrollback when needed; a concurrent pane resize may shorten that frame.
- Guide mode sends that frame's `Text` to Haiku in one non-streaming request,
  with no model history or structured response and no second request. It
  renders the result as compact conversational prose with short, single-idea
  paragraphs. Shared work uses a collaborative "we" voice; "you" is reserved
  for actions that belong to the reader alone.
- Haiku names a tool, project, account, or person only when the terminal text
  visibly establishes that identity. Model identifiers are never user identities.
- Snapshot mode renders the same frame through Chromium into a full-bleed
  430x932 logical-pixel image at 3x density.
- Terminal content is untrusted data for Haiku, not instructions or authority.
- A guide anchor includes `🖼️` only when Chromium passed startup readiness. A
  snapshot anchor includes `🗣️` only when Haiku is configured. These produce
  one-off replies and never replace the canonical anchor.
- Both modes append locally extracted references. `paths` contains at most four
  existing absolute or home-relative regular files/directories. `links`
  contains at most four valid HTTP(S) URLs. Engram never asks Haiku to generate
  references or fetches an extracted URL.
- `/raw` preserves the visible pane's physical wrapped lines and attributes.
  `/dump` streams physical full scrollback to an attachment.
- `/raw` and `/dump` stop before Telegram's 50 MiB upload ceiling.

## Closing

- Sessions record whether Engram created their window. Legacy sessions without
  provenance are treated as attached.
- `/close <id>` kills only an Engram-created window after identity validation.
  Attached and legacy sessions are merely untracked.
- Inline close requires a separate, expiring confirm/cancel callback. A failed
  tmux close does not mark a session closed.
- Closed and lost sessions do not refresh or retain input controls.
