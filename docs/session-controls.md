# Session Controls

Engram presents each expanded watch as one pinned Telegram message. This guide
explains the controls around that message. The binding behavior remains in
[`requirements/telegram.md`](../requirements/telegram.md) and
[`requirements/tmux.md`](../requirements/tmux.md).

## Input

Reply to a session's pinned message to send a shell command to its pane. Add an
extra leading slash when the input itself begins with `/`: replying with
`//clear` sends `/clear` and presses Enter.

The explicit command paths are:

- `/send <id> <text>` sends a shell command and presses Enter;
- `/text <id> <text>` sends one literal line without Enter; and
- `/key <id> <keys...>` sends validated tmux key names.

When a guide provider is configured, `⌨️` accepts a natural-language key
description such as `up three times, Enter, then Ctrl+C`. Engram shows the
normalized sequence and exact target in a separate confirmation. The model can
propose keys but cannot send them. Unsupported keys, ambiguity, malformed or
oversized proposals, stale cards, and expired confirmations fail closed.

Without a guide, anchors expose direct `Esc`, `Escx2`, `^C`, `^D`, and `Enter`
controls. Snapshot mode also exposes arrow keys. `/key` remains the exact expert
path in both modes.

## Saved Input

`/remember <name> <text>` stores an exact input template. Use
`{engram:name}` in an ordinary reply, a new-session message, `/new`, `/send`, or
`/text`. Engram expands the body once before the normal tmux input checks.

```text
/remember review-panel Imagine the ideal panel to review this pull request...
Please {engram:review-panel}
```

Use `/remember` to list names, `/remember <name>` to inspect one, `/forget
<name>` to remove it, and `/templates export` to download one consistent JSON
snapshot. Other brace forms remain literal. Templates do not recurse and do
not expand from voice notes or terminal output.

`templates.json` contains exact plaintext. Expansion audit events retain the
template name and destination, not the body. Expanded shell input may still
appear in tmux history and bounded recovery previews. `/text` remains one-line
after expansion, so a template cannot submit staged input by inserting a line
break.

## Current And Alternate Views

Each expanded watch has one current pinned message. A guide message or snapshot
can also produce one-off alternate views through `🖼️ View` or `🗣️ Talk`. The
latest alternate of each kind may route a reply to the same watch. Replacing an
alternate makes its predecessor stale; a stale reply returns an error and never
reaches tmux.

Snapshots render the frame's physical rows. Guides interpret its logical text.
Inline `📄 Raw` returns the process-local plain-text companion for its current
media message. `/raw <id>` performs a new bounded capture. `/dump <id>` streams
the pane's retained history with soft wraps joined.

## Hide And Show

`➖ Hide` moves a running pinned message into the shared, pinned `Collapsed
sessions` shelf. The shelf shows one cached status line per watch and one `➕
Show` control. Collapsed watches perform no capture, guide, Chromium, raw/dump,
or alternate-view work and expose no reply or terminal-control route.

Engram keeps the individual message current until the shelf has been rendered
and pinned. `➕ Show` acknowledges immediately and restores each member under
the current presentation. A member whose controls cannot be restored returns
to the shelf instead of becoming inert. The shelf remains recoverable until
all individual messages are durable.

Replies to retired pre-collapse messages are stale and point back to the shelf.
Lost watches show recovery controls instead of promising a refresh that cannot
run.

## Refresh And Presentation

Guide mode uses one non-streaming model request for a refresh. Later renderings
of the same stable capture may use deterministic added and removed lines plus
the previous prose for continuity. Those hints never override the current
frame. A capture-boundary change, weak alignment, manual refresh, mode switch,
reattachment, restart, or failed delivery discards continuity.

Snapshot mode edits the pinned image when its styled frame or derived caption
changes, at most once every ten seconds. The manual refresh renders
immediately, even when the capture is unchanged. A failed browser probe appears
in `/status`; bounded retries can restore snapshot controls without a restart.

`/mode guide` and `/mode snapshot` migrate expanded messages only when the
target capability is available. The persisted choice changes after replacement
delivery succeeds. A failed replacement leaves the prior presentation current.

## Files And Links In A Frame

Both presentations can append bounded local references found in the pane:

- numbered existing regular files under `files`; and
- syntactically valid HTTP(S) URLs under `links`.

Directories, symlinks, missing files, credential-shaped paths, embedded URL
credentials, and unsafe query values are omitted or redacted. File buttons use
the same guarded upload path as `/download` and remain bound to the exact list
on the current message. Engram never fetches or endorses an extracted URL.

## Close And Recovery

`/close <id>` kills a window created by Engram after identity validation. For
an attached or legacy watch, it only stops tracking the pane and leaves the
tmux window running. Inline close always requires a separate confirmation.

If a provider session loses its tmux pane, `/recovery` produces a deterministic
plan and `/resume` creates a replacement pane for the exact stored Codex or
Claude session identity. Engram never replays an observed launch automatically.

## Nested Environments

Every watched pane advertises terminal-native capabilities through tmux pane
options:

```sh
tmux show-options -pv @engram
tmux show-options -pv @engram_watch_id
tmux show-options -pv @engram_notify
tmux show-options -pv @engram_artifact
tmux show-options -pv @engram_codex
tmux show-options -pv @engram_github
```

`@engram` is the versioned commit marker. Ignore auxiliary values unless that
marker is present and its watch ID matches `@engram_watch_id`. Engram removes
the options when an attached pane is untracked and restores them for active
watches after restart.

A program already running in the pane can request the outer user's attention:

```sh
engram signal "Tests finished; two failures need attention."
```

This writes one bounded, versioned record and a terminal bell to the controlling
terminal. A host that captures command stdout before visibly relaying it into
the pane can use `engram signal --stdout`. Neither form creates a transport for
detached jobs.

The outer Engram observes the record through its ordinary bounded frame and
sends a redacted notification. Replying to the newest notification routes to
the same outer pane. Any pane writer can forge a signal, so its payload remains
untrusted terminal text. Delivery is best effort and rate-limited; fast output
can move the record out of the observed frame.

A producer can associate an existing regular file with the same frame by
printing a local `file://` URI before signaling. Engram may display a guarded
file button, but it does not open, read, upload, or execute the file
automatically. See
[`requirements/upstream-signals.md`](../requirements/upstream-signals.md) for
the exact record, wrapping, file, reply, and recovery contract.
