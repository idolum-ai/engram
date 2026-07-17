# tmux Requirements

tmux is the source of terminal truth.

Engram requires tmux 3.2 or newer for byte-length metadata formats.

## Session Selection

- If `ENGRAM_TMUX_SESSION` is configured, Engram uses it and creates it if
  missing. Otherwise Engram uses the first existing session, or creates
  `engram-<chat-id>` when no session exists.
- Engram resolves the exact literal session name from `list-sessions`, then
  creates windows through tmux's immutable `$session_id`. Numeric names such as
  `0`, prefixes, and target-like names must not resolve to another session.
- Configured names containing `:` or `.` are rejected because tmux canonicalizes
  those separators during session creation.

## Windows And Attachments

- A top-level non-command Telegram message creates a new tmux window.
- `/attach <target>` tracks an existing target's active pane.
- Tmux metadata uses byte-length-framed fields rather than printable or control
  delimiters. Session, window, pane, and capture records must reject malformed,
  partial, trailing, or invalid immutable-identity data; Engram never pads a
  record or persists an empty identity after a successful attach.
- `/sessions` shows native tmux sessions and immutable pane identities with
  attach buttons for untracked panes and explicit reattach buttons when a
  persisted watch belongs to an older tmux server incarnation.
- Attach callbacks carry `%pane_id`, not mutable indexes.
- Each watch stores a random tmux server incarnation in addition to immutable
  pane/window IDs. Before input, capture, or cwd lookup, Engram verifies all
  three identities. Server incarnation and pane metadata are sampled in one
  tmux call; attach also brackets target resolution with server-incarnation
  reads so a restart cannot combine identities from two servers. Destructive
  close evaluates and kills in one tmux command queue so a concurrent pane move
  cannot redirect it. A mismatch marks the session lost; transient command
  failure does not.
- Pane-bound input, capture, scrollback, and destructive close cross the private
  terminal-mechanics boundary, which has no Telegram, state, or presentation
  dependency and validates immutable identity immediately before the operation.
- A lost anchor can recover automatically only when its exact server, pane, and
  window identity returns. `/attach` is the explicit authority to rebind an old
  watch to the selected pane after a tmux restart; it adopts the pane as an
  attached window and never inherits destructive close authority.

## Input

- Replies to the canonical anchor send literal text and Enter to its pane.
  The latest conversational and screenshot replies are aliases to the same
  pane. Retired anchors and stale alternate replies do not route input.
- Every literal-text and key effect executes behind a tmux-side server/window
  identity condition. Literal text crosses that command boundary through a
  random temporary tmux buffer. When the foreground application requests
  bracketed-paste mode, tmux wraps the complete buffer so multiline or long
  Telegram input arrives as one paste before Engram sends one Enter. If tmux
  restarts between text and Enter, the second effect fails closed instead of
  reaching a reused pane ID.
- A reply beginning `//` removes one slash and sends the resulting slash-led
  input downstream. `/text` omits Enter; `/key` sends validated tmux key names.
- Live anchors include `Esc`, `Escx2`, `^C`, `^D`, and `Enter`. Snapshot anchors
  additionally include the four arrow keys in a distinct `← ↑ ↓ →` row.
  `Escx2` waits 500 ms between Escape keys.

## Capture And Presentation

- Both anchor modes use the same ANSI-preserving `CaptureStyled` result. It
  targets and caps at 64 rows. Panes no taller than that use available recent
  scrollback and end at the pane bottom. Taller panes first select the densest
  meaningful 64-row interval from the visible screen so full-screen programs
  with large blank regions do not render an empty frame. The final physical ANSI
  and joined captures use that exact interval in one tmux command batch. Engram
  samples identity, dimensions, foreground
  command, alternate-screen state, and copy-mode state immediately before and
  after the capture and rejects a frame when any sampled boundary changes.
  These are endpoint observations, not process-generation identity: tmux cannot
  reveal a same-name restart or an enter-and-exit transition between samples.
  Full current-frame evidence therefore remains mandatory even when continuity
  hints are admitted.
- `CaptureStyled` also carries a logical-text view made with tmux's joined-wrap
  semantics over the same coordinates. The physical and joined captures execute
  in one tmux command batch so signal parsing, guide text, references, and
  snapshot pixels do not come from separately timed observations.
- `CaptureStyled` extracts at most 16 distinct OSC 8 hyperlink targets from the
  physical ANSI capture in appearance order. It accepts BEL, ESC-ST, and C1-ST
  string terminators, rejects invalid UTF-8 and embedded newlines or NULs, and
  caps each target at 2 KiB. Hyperlink controls remain absent from semantic text
  and rendered terminal pixels.
- A running watched pane publishes `@engram`, `@engram_watch_id`,
  `@engram_notify`, and `@engram_artifact` tmux pane user options behind the
  same immutable server/window binding guard used for input. `@engram` is the
  commit marker: Engram clears it before changing auxiliary values, publishes
  it last, and clears it first on removal. Consumers ignore auxiliary options
  unless the marker is present and its watch ID agrees with `@engram_watch_id`.
  The versioned summary advertises the remote surface; the other options give a human-readable
  notification command and the standard OSC 8 artifact sequence. Startup
  repairs metadata for persisted running watches; normal unwatch or
  attached-pane untracking removes it without changing the pane program,
  environment, title, or other options.
- Deterministic reference extraction uses the joined logical-text view after
  terminal-authored upstream records have been removed. Unmatched closing
  wrappers are removed; other terminal punctuation is retained. URL candidates
  preserve their scheme, host, path, fragment, and appearance order subject to
  structural and best-effort credential redaction. Malformed query strings fail
  closed, and a URL whose authority would require redaction is omitted. Engram
  does not canonicalize, rank, or otherwise translate URLs for particular hosts.
  At most the first four distinct visible HTTP(S) URLs are shown.
- Both anchor modes enumerate regular local files in a code block. Links stay
  outside code blocks so Telegram can make them directly navigable.
  Presentation tests must preserve that distinction.
- Validated OSC 8 targets and visible literal `file://` URIs are considered
  before references heuristically found in visible text. `file://` targets must be absolute, local or `localhost`,
  query- and fragment-free, existing regular files that are not symlinks.
  Explicit HTTP(S) targets use the same structural validation and credential
  redaction as visible URLs. All existing count and byte limits still apply.
- Guide mode sends every frame's complete joined logical text, with upstream
  records, the trailing model-status footer, and a small allowlist of paired
  Codex placeholder prompts removed, to the selected guide provider in one
  non-streaming request. The guide-only footer and placeholder cleanup does not
  alter raw captures, screenshots, references, or hashes; upstream-record
  removal happens earlier and intentionally excludes those records from every
  presentation view. Within a stable and
  strongly aligned capture boundary, Engram also supplies the previous
  rendering, deterministic added and removed lines, and bounded unchanged
  neighbors. These fields direct attention and conversational tone; they never
  replace or override the complete current terminal evidence. Engram does not
  remember submitted Telegram input for this purpose.
- A different binding, foreground command, pane size, alternate-screen state,
  copy-mode state, weak line alignment, manual refresh, mode switch,
  reattachment, or service restart discards conversational hints. Continuity
  is process-local, isolated per tracked window, advanced only after canonical
  delivery, and never persisted or shared across windows. One-off alternate
  renderings do not mutate it.
- Every guide rendering still uses exactly one non-streaming model request,
  with no model API history or second request. It renders
  compact conversational prose with short, single-idea paragraphs. Engram
  deterministically bounds a completed response to at most 180 words before
  delivery. When Chromium is ready, the same response carries a private trailer
  containing at most two short verbatim evidence excerpts. Engram removes that
  trailer from user-facing prose and treats it only as an untrusted crop hint.
  Shared work uses a collaborative "we" voice; "you" is reserved for actions
  that belong to the reader alone.
- The guide names a tool, project, account, or person only when the terminal text
  visibly establishes that identity. Model identifiers are never user identities.
- Snapshot mode renders the same frame through Chromium into a full-bleed image
  at 3x density. Narrow frames use a 430x932 logical-pixel canvas. Rows wider
  than the readable viewport soft-wrap at up to 100 columns; the logical width
  expands only enough to retain the 7px font and the height grows to contain all
  wrapped rows. No captured column may be silently clipped. The worst supported
  400-column, 64-row frame remains within Telegram's photo dimension limits.
- Guide mode may render its canonical anchor as a compact evidence photo card
  from the same captured frame, with bounded prose below the media.
  Every model excerpt must first match one unique range in the cleaned semantic
  text sent to the provider, then one unique physical row range after whitespace
  normalization. Engram adds at most two context rows on each side, highlights
  only matched rows, and rejects a crop spanning more than 18 rows. Ambiguous,
  fabricated, or widely separated model evidence falls back to the last
  changed on-screen physical-row region under the same continuity boundaries, then to
  the last meaningful non-empty terminal block capped at 10 rows. The crop
  footer identifies `quoted terminal text`, `changed terminal region`, or
  `current terminal tail`; tail rows are not highlighted. A crop carries the
  active SGR state from preceding rows. Compact crops preserve every complete
  selected physical row and soft-wrap it at 71 terminal cells; no horizontal
  viewport or column offset may discard text. The highlight border begins at
  the content edge and covers every wrapped visual fragment belonging to a
  highlighted physical row. Tabs, combining marks, and wide Unicode characters
  use terminal-cell widths. Crops enforce the accessible
  contrast floor regardless of the full-snapshot theme. If the styled tail
  cannot be delivered safely, Engram renders the same bounded range as redacted
  plain text. Empty terminals use a quiet `guided view` frame. Engram never
  preserves stale pixels or falls back to a larger automatic screenshot.
- The exact plain text corresponding to the selected guide rows, before visual
  soft-wrapping, is retained
  only in process memory and is available through `📄 Raw` while that canonical
  message remains current.
- Terminal content is untrusted data for the model, not intended instructions or
  authority; prompt-injection resistance is best effort and model output is
  never executed automatically.
- A guide anchor includes `🖼️ View` only when Chromium passed startup readiness. A
  snapshot anchor includes `🗣️` only when a guide is configured. These produce
  one-off replies and never replace the canonical anchor.
- Both modes append locally extracted references. `files` contains at most four
  existing absolute regular files after home expansion; directories, symlinks,
  missing files, and credential-shaped paths are omitted. `links` contains at
  most four valid HTTP(S) URLs. Engram never asks the model to generate
  references or fetches an extracted URL.
- `/raw` preserves the visible pane's physical wrapped lines and attributes.
  `/dump` streams physical full scrollback to an attachment. Both captures are
  conditionally executed against the stored server, window, and pane identity
  in the same tmux command queue; a queued request is canceled if that binding
  changed before its worker began.
- `/raw` and `/dump` stop before Telegram's 50 MiB upload ceiling.
- `engram inspect frame <watch-id>` captures at most 64 recent plain-text rows
  without tmux paste buffers, strips terminal controls, and caps stdout at
  128 KiB. Its capture is binding-conditional. It accepts an Engram watch ID,
  never an arbitrary tmux target.

## Upstream Signals

- A nested process may request attention by writing the bounded terminal record
  and bell defined in [`upstream-signals.md`](upstream-signals.md). The outer
  tracked pane remains the routing and identity boundary.
- Optional bell acceleration uses tmux's window state; signal discovery uses
  the existing pane capture path. Engram does not attach to, enumerate, or send
  input directly to an inner tmux server.

## Closing

- Sessions record whether Engram created their window. Legacy sessions without
  provenance are treated as attached.
- `/close <id>` kills only an Engram-created window after identity validation.
  Attached and legacy sessions are merely untracked.
- Inline close requires a separate, expiring confirm/cancel callback. A failed
  tmux close does not mark a session closed.
- Closed and lost sessions do not refresh or retain input controls.
