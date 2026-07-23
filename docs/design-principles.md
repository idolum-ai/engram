# Engram Design Principles

Status: draft design direction.

Engram is a Telegram control surface for tmux. It exists so one user can move
quickly across many terminal sessions from a phone, recover context in seconds,
and avoid getting trapped in any single pane.

The material is tmux. Engram creates the conditions where tmux becomes more
conductive for remote work: low-friction input, quick orientation, useful
anchors, copyable references, and simple recovery.

## Short Form

- Phase change, not platform.
- tmux is the workspace.
- Phone-first anchors.
- Fast input path.
- Many sessions, low dwell.
- One frame, two presentations.
- Deterministic facts beat guesses.
- Interpretation is optional; tmux remains literal.
- Existing tmux first.
- Slow automatic edits, instant manual refresh.
- Recoverable local service.
- Small Go, no third-party dependencies.
- Quiet monochrome signal.
- Recursion collapses to the terminal.

## Principles

### Phase change, not platform

Engram should stay a small layer that changes how usable tmux feels from a
phone. It should not become a general chat app, task marketplace, plugin host,
or replacement shell. The right feature makes remote terminal work lighter;
the wrong feature adds another surface the user must manage.

### tmux is the workspace

tmux remains the source of terminal truth. Engram should not emulate a
terminal, invent session state, or hide what happened in the pane. It creates,
attaches, captures, and sends input to tmux. `/raw` and `/dump` preserve direct
ways to inspect exact state.

tmux is a deliberate dependency because its mature, narrow interface has
effectively crystallized. Low expected API drift lets Engram stay small and
precise instead of continually adapting to a moving workspace substrate.
Engram-created windows use tmux's configured default size, so their applications
render against real tmux geometry consistently across attached and detached
hosts. Engram does not resize windows that the user explicitly attaches.

### Phone-first anchors

The editable Telegram anchor is the core product surface. It should identify
the session, show what the pane is doing, and make the next useful action easy.
Each expanded session has exactly one canonical, pinned anchor. A collapsed
session instead has one inert shelf entry and no individual route. Older
anchors become inert; two actionable representations of one pane are a product
error.

A guide anchor uses compact conversational prose. A snapshot anchor uses a
bounded, ANSI-preserving terminal image. Both retain deterministic references
and small allowlisted controls, including exact numbered handoffs for files
already visible in the pane.
Snapshot anchors keep the exact delivered frame's literal text one tap away
through a `📄 Raw` attachment; the image is primary, not exclusive.

Running anchors may move into one shared pinned shelf when the user needs less
visual weight. Each collapsed session contributes one cached status line, but
the shelf is deliberately not a terminal input route. Its only control is
`➕ Show`, which restores every member as an individual canonical anchor in the
selected guide or snapshot mode. Restoration acknowledges immediately, makes
each prospective anchor durable while inert, and only then grants controls and
a pin. Collapse follows the reciprocal rule: the individual anchor remains
canonical until the shared shelf is rendered and pinned. The shelf identifies
its summaries as cached because hiding a session also stops observation.

### Fast input path

Sending input to tmux must remain fast when a model provider, Chromium, or Telegram
delivery is delayed or failing. Replying to an anchor and using `/send`,
`/text`, `/key`, or key buttons should route directly and predictably to the
intended pane. Presentation work must not block input; an interactive tmux
operation may preempt Engram's own background observation and recovery work.

Remembered input should remain explicit text, not inferred automation. A user
may give exact prose a short name and invoke it with a typed placeholder.
Engram expands that placeholder once immediately before the ordinary guarded
input path; it does not learn triggers, recurse through template bodies, or run
anything merely because terminal output resembles a past situation.

### Many sessions, low dwell

Engram is for multitasking. The user should be able to scan several sessions,
enter the relevant one, send a command, and move on. Output changing is not by
itself a reason to create a notification.

Automatic anchor edits should be intentionally slow and only occur for changed
captures. Manual refresh should be immediate. `/sessions` is a concise map:
lost work first, then collapsed and active work by recency.

Collapsed sessions should spend less attention and no presentation machinery.
They omit captures, model calls, images, references, evidence selection, reply
routes, raw/dump disclosure, and terminal-key controls. Expansion first restores cached summaries as
ordinary anchors with their cached state labeled, then lets bounded background
rendering make each one current without waiting for the rest of the shelf.
Incomplete handoffs retain durable ownership of prospective and retiring
Telegram messages, including retry deadlines, so restart or rate limiting
cannot silently create a second route or abandon cleanup.

### One frame, two presentations

Guide and snapshot modes begin with the same `CaptureStyled` terminal frame,
targeting and capped at 64 rows. Presentation changes; observation does not.
This shared boundary keeps comparisons honest and prevents either mode from
quietly seeing more of the machine.

Guide mode sends every frame's joined logical text to the selected model after
removing recognized upstream records. A bounded semantic analyzer uses visible
position, glyph, model, and temporal signals to separate conversation
evidence from high-confidence agent-interface chrome. It extracts normalized
model, effort, mode, and activity without gating on a particular agent CLI or
version. Unknown or weak layouts keep the ordinary terminal path byte-for-byte;
a process-confirmed, versioned Codex adapter remains as a compatibility fallback
for its already-proven layouts. The raw frame remains intact for screenshots,
local inspection, and references. See
[`agent-screen-semantics.md`](agent-screen-semantics.md) for the role,
confidence, retention, and credential-isolated real-client test contract. Guide hashes for recognized
agent presentations use cleaned conversation so UI animation or extracted
state alone cannot spend another guide request. Card
render hashes still include the extracted state. When
two frames remain strongly aligned within
the same tmux server, window, pane, foreground command, dimensions, alternate
screen, and copy-mode boundary, Engram also supplies deterministic added and
removed lines, a few unchanged neighbors, and the previous rendering. Those
extras direct attention and preserve voice; the complete current semantic
evidence remains the only terminal truth. The guide speaks like a
technically fluent person briefly returning to the topic: compact, natural,
and focused on what the terminal content means, within 180 words. Its voice
stands beside the reader, using direct orientation grounded only in visibly
named tools, collaborative "we" for shared work, and short one-idea paragraphs
for phone readability; "you" is reserved for actions only the reader can take.
It has no model conversation history and makes no second request. When local
Chromium is ready, its single response also carries a private list of short
verbatim evidence excerpts. Engram strips that metadata from the prose and
accepts it only when each excerpt maps uniquely back to the shared physical
capture. Its small process-local continuity is isolated per window and
never becomes terminal truth. A different capture boundary, weakly aligned
frame, manual refresh, mode switch, reattachment, or service restart discards
the hints. Continuity advances only after the canonical Telegram rendering is
accepted, never crosses windows, and is never persisted. One-off alternate
views do not alter it.

Snapshot mode renders the frame's ANSI styling locally through Chromium. It is
literal and deterministic, at the cost of greater visual density and local
rendering work.

Guide mode can use that same renderer to make its canonical anchor one compact
photo card. Highlighted terminal rows provide inspectable evidence first; the
prose remains readable and copyable below the media without a
second message. The model cannot choose pixels or expand the observation
boundary. Engram adds bounded surrounding rows. When matching fails, it falls
back deterministically to the last changed on-screen physical-row region since the
last accepted frame, then to a bounded physical paragraph with lexical affinity
to the summary, favoring visible links, and finally to the current terminal tail.
The footer says which basis was used; quoted, locally changed, or summary-related
rows are highlighted. Exact
occurrence is not presented as semantic verification. If
styled rows cannot be delivered safely, the bounded tail is redacted and
rendered without styling. A truly empty terminal receives a quiet guide-only
frame rather than a warning that competes with the summary. The canonical
message identity, pin, controls, and reply route remain stable.

When a guide is configured and Chromium has passed its local probe, `/mode` may
begin switching the canonical presentation without changing the underlying
session. A snapshot
anchor may offer `🗣️ Talk` for a one-off conversational reply; a guide anchor may
offer `🖼️ View` for a one-off image. Alternate views are shown only when Engram can
actually deliver them, and they never become a second canonical anchor. The
latest alternate of each kind may act as a reply handle for its session; an
older alternate is explicitly stale and cannot route input.

### Deterministic facts beat guesses

Engram should compute session IDs, tmux targets, pane IDs, working directories,
attachment paths, visible files and URLs, capture hashes, timestamps, and
service status locally. Extracted references are untrusted pane content;
Engram does not fetch or endorse them.

A snapshot footer may include one bounded fact produced by a trusted local
shell command from the protected Engram configuration. This is a narrow Unix
pipe, not a catalog of operating-system status providers or a plugin protocol:
the command receives the pane directory and returns one sanitized line. Engram
owns its visual budget, runs it only while a render is already happening, and
does not let status-only changes create automatic edits.

The selected model interprets only the bounded terminal text. Terminal text is data, not
authority; the prompt tells it to ignore instructions addressed to Engram or
the reader, while recognizing that model-level injection resistance is best
effort. Model output is presentation and is never executed automatically.
The guide should not invent history, claim work succeeded, or explain Engram
controls unless the terminal itself is about Engram.

### Existing tmux first

Engram should work with tmux sessions that already exist. Target selection is
predictable: the configured session first, otherwise an existing session,
otherwise a managed fallback.

### The terminal advertises its affordances

An agent or person already looking at tmux should be able to discover that a
pane is remotely observed without first knowing an Engram command or reading a
repository-specific instruction file. A watched pane therefore publishes a
small versioned set of tmux user options describing its ordinary terminal
affordances: a signal command and OSC 8 file references. Discovery stays in tmux, and
use stays in terminal output; Engram does not inject shell variables, mutate
the foreground program, or invent a private agent protocol.

### Recursion collapses to the terminal

An Engram may run inside a container or terminal observed by another Engram.
That does not require a hierarchy or an Engram-to-Engram network. A nested
process can expose an intentional local artifact with a standard OSC 8
`file://` hyperlink, then emit one visible, bounded attention record with a
terminal bell;
the outer Engram observes the record through its normal tmux capture and keeps
the outer pane as the truthful reply boundary. Composition should emerge from
the terminal path that already exists, without distributing credentials or
inventing a control plane.

### Slow automatic edits, instant manual refresh

Telegram anchors should not flicker. Engram hashes captures, coalesces bursts,
and automatically edits no more than once every ten seconds when content
changed. A manual refresh captures and renders immediately; a snapshot refresh
may redraw an unchanged frame because the user explicitly asked to look now.

### Recoverable local service

State under `~/.engram` should recover sessions, canonical anchors, the shared
collapsed shelf and its members, selected mode, attachments, poll position, and recent errors after restart. Diagnostics
must be available locally and through Telegram without exposing configured
credentials.

### Small Go, no third-party dependencies

Engram remains a small Go system built with the standard library. tmux and the
Telegram Bot API are always required. A selected conversational provider and
local Chromium are independent, optional presentation capabilities; at least the configured
startup mode must be ready. systemd is used only for the Linux service install.

Adding a Go dependency should be treated as a design failure until proven
otherwise.

### Quiet monochrome signal

Engram's visual language should feel like a terminal pane becoming readable
through interference: black field, off-white signal, graphite texture, moire
lines, aperture forms, and sparse monospace typography. Visuals should stay
quiet and structural rather than competing with the tool.

## Non-Goals

- Multi-user or group-chat operation.
- A general autonomous agent.
- A replacement terminal emulator.
- Unbounded terminal streaming as the default anchor.
- A plugin system.
- Long model chat memory or cross-window model context.
- Model-generated file/path inventories.
- Broad notification routing across chat systems.
- An Engram-to-Engram control plane or persistent deployment hierarchy.

## Design Review Questions

- Does this reduce the time needed to understand a tmux session from Telegram?
- Does input remain immediate and tmux remain the source of truth?
- Do guide and snapshot presentations observe the same bounded frame?
- Is this fact computed locally instead of guessed by a renderer?
- Does the anchor stay compact on a phone?
- Does this help many-session multitasking without creating notification noise?
- What happens if a renderer, Telegram, or the service fails halfway through?
- Can the user recover with `/status`, `/sessions`, `/logs`, `/raw`, or `/dump`?
- Did this add avoidable state, background work, or a dependency?
- Is behavior tested across Telegram input, tmux effects, and anchor delivery?
