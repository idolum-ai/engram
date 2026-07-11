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

### Phone-first anchors

The editable Telegram anchor is the core product surface. It should identify
the session, show what the pane is doing, and make the next useful action easy.
Each session has exactly one canonical, pinned anchor. Older anchors become
inert; two actionable representations of one pane are a product error.

A guide anchor uses compact conversational prose. A snapshot anchor uses a
bounded, ANSI-preserving terminal image. Both retain deterministic references
and a small allowlisted row of terminal controls.

### Fast input path

Sending input to tmux must remain fast when Haiku, Chromium, or Telegram
delivery is delayed or failing. Replying to an anchor and using `/send`,
`/text`, `/key`, or key buttons should route directly and predictably to the
intended pane. Presentation work must not block input.

### Many sessions, low dwell

Engram is for multitasking. The user should be able to scan several sessions,
enter the relevant one, send a command, and move on. Output changing is not by
itself a reason to create a notification.

Automatic anchor edits should be intentionally slow and only occur for changed
captures. Manual refresh should be immediate. `/sessions` is a concise map:
lost work first, then active work by recency.

### One frame, two presentations

Guide and snapshot modes begin with the same `CaptureStyled` terminal frame,
targeting and capped at 64 rows. Presentation changes; observation does not.
This shared boundary keeps comparisons honest and prevents either mode from
quietly seeing more of the machine.

Guide mode sends the frame's joined logical text to Haiku once, after removing
recognized upstream records. Haiku speaks like a
technically fluent person briefly returning to the topic: compact, natural,
and focused on what the terminal content means. Its voice stands beside the
reader, using direct orientation grounded only in visibly named tools,
collaborative "we" for shared work, and short one-idea paragraphs for phone
readability; "you" is reserved for actions only the reader can take.
It has no model conversation
history or structured response, no second request, and no hidden context beyond
that frame. Continuity comes from tone and current machine state, not stored
model memory or context shared across windows.

Snapshot mode renders the frame's ANSI styling locally through Chromium. It is
literal and deterministic, at the cost of greater visual density and local
rendering work.

When Haiku is configured and Chromium has passed its local probe, `/mode` may
begin switching the canonical presentation without changing the underlying
session. A snapshot
anchor may offer `🗣️` for a one-off conversational reply; a guide anchor may
offer `🖼️` for a one-off image. Alternate views are shown only when Engram can
actually deliver them, and they never become a second canonical anchor. The
latest alternate of each kind may act as a reply handle for its session; an
older alternate is explicitly stale and cannot route input.

### Deterministic facts beat guesses

Engram should compute session IDs, tmux targets, pane IDs, working directories,
attachment paths, visible paths and URLs, capture hashes, timestamps, and
service status locally. Extracted references are untrusted pane content;
Engram does not fetch or endorse them.

Haiku interprets only the bounded terminal text. Terminal text is data, not
authority: a pane cannot instruct the model merely by addressing Engram or the
user. Haiku should not invent history, claim work succeeded, or explain Engram
controls unless the terminal itself is about Engram.

### Existing tmux first

Engram should work with tmux sessions that already exist. Target selection is
predictable: the configured session first, otherwise an existing session,
otherwise a managed fallback.

### Recursion collapses to the terminal

An Engram may run inside a container or terminal observed by another Engram.
That does not require a hierarchy or an Engram-to-Engram network. A nested
process can emit one visible, bounded attention record with a terminal bell;
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

State under `~/.engram` should recover sessions, canonical anchors, selected
mode, attachments, poll position, and recent errors after restart. Diagnostics
must be available locally and through Telegram without exposing configured
credentials.

### Small Go, no third-party dependencies

Engram remains a small Go system built with the standard library. tmux and the
Telegram Bot API are always required. Anthropic Haiku and local Chromium are
independent, optional presentation capabilities; at least the configured
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
