# Engram Design Principles

Status: draft design direction.

Engram is a Telegram control surface for tmux. It exists so one user can move
quickly across many terminal sessions from a phone, recover context in seconds,
and avoid getting trapped in any single pane.

The material is tmux. Engram creates the conditions where tmux becomes more
conductive for remote work: low-friction input, quick orientation, useful
anchors, copyable paths, and simple recovery.

These principles are not feature marketing. They are the shape Engram should
keep as it grows.

Engram turns persistent machine state into an attention-aware interface. The
environment holds the memory; Haiku interprets its visible trace; the human
intervenes only when judgment is needed.

## Short Form

- Phase change, not platform.
- tmux is the workspace.
- Phone-first anchors.
- Fast input path.
- Many sessions, low dwell.
- Durable handoffs, not labels.
- Deterministic facts beat guesses.
- Haiku guides; Engram renders.
- Existing tmux first.
- Slow automatic edits, instant manual refresh.
- Recoverable local service.
- Small Go, no third-party dependencies.
- Quiet monochrome signal.

## Principles

### Phase change, not platform

Engram should stay a small layer that changes how usable tmux feels from a
phone. It should not become a general chat app, a task marketplace, a plugin
host, or a replacement shell.

The right feature makes remote terminal work feel lighter. The wrong feature
adds another surface the user must manage.

### tmux is the workspace

tmux remains the source of terminal truth. Engram should not try to emulate a
terminal, invent session state, or hide what actually happened in the pane.

Engram creates, attaches, captures, and sends input to tmux. When exact state
matters, `/raw` and `/dump` are the escape hatches.

### Phone-first anchors

The editable Telegram anchor is the core product surface. It should let the
user answer three questions quickly:

1. Which session is this?
2. What is the pane doing?
3. What is the next useful action?

Anchors should be compact, stable, and easy to scan. They should include the
session handle, state, title, last input preview, Haiku status, one recommended
action, deterministic paths that currently exist, a refresh button, and
an on-demand terminal image button, and a small allowlisted row of common
terminal keys.

The image is a deterministic glance surface for terminal layouts that lose
meaning in prose: diffs, TUIs, dashboards, prompts, and spatial output. It
should use a phone-native canvas, include bounded recent scrollback when space
allows, and remain subordinate to the text anchor rather than becoming a second
live control surface.

Each session should have exactly one live anchor. Watched live anchors should be
pinned for navigation. When attention moves a session forward in chat history,
the new message becomes the live anchor and the predecessor becomes a compact,
unpinned, control-free trace. Two simultaneously actionable representations of
one pane are a product error.

### Fast input path

Sending input to tmux must stay fast even when summaries are delayed, skipped,
or failing. Telegram message handling should not wait for Haiku unless the
message itself is asking for a summary.

After an intervention, Engram should observe the pane again. When it cannot
establish an effect, it should preserve that uncertainty rather than imply that
the requested outcome occurred.

Input acknowledges a handoff; it does not erase one. Resolution belongs to the
next settled observation, not to the optimistic assumption that sending a key
must have worked.

Replying to an anchor should route to the intended pane. `/send`, `/text`,
`/key`, and anchor key buttons should remain direct, unsurprising ways to steer
a session.

### Many sessions, low dwell

Engram is for multitasking. The user should be able to scan several sessions,
tap into the one that needs attention, send one command, and move on.

Progress that needs no judgment should remain quiet. Output changing is not by
itself a reason to demand attention.

Automatic anchor edits should be intentionally slow and only happen when there
is useful new information. Manual refresh should be immediate. `/sessions`
should act as an attention-ordered map of active and attachable work, not as a
verbose report.

### Durable handoffs, not labels

Engram should not classify every screenshot into a terminal phase or ask the
user to review whatever the model finds ambiguous. A handoff is narrower: a
specific, cited boundary where the apparent work cannot usefully advance until
the user supplies judgment, input, approval, correction, credentials, or the
next choice.

A model suggestion is not yet a handoff. Engram should wait for compatible
settled observations and bind the handoff to exact capture evidence. Opening a
handoff rotates the session's single live anchor forward rather than creating a
second static notice. User input acknowledges it. Later evidence updates that
anchor, resolves the handoff, replaces it with a materially different need, or
reopens it when the intervention had no established effect.

This lifecycle is the attention interface. `/sessions` should present waiting
handoffs before quiet sessions, while pinned anchors keep every watched session
reachable independent of chat history. Changing output, uncertain
interpretation, and bare idleness are not reasons to rotate an anchor.

### Deterministic facts beat guesses

Engram should generate local facts itself whenever it can: session IDs, tmux
targets, pane IDs, current working directories, attachment paths, visible file
paths, capture hashes, timestamps, and service status.

Haiku should not be asked to infer facts Engram can compute. The model should
interpret terminal content; Engram should render known metadata.

### Haiku guides; Engram renders

Haiku's job is to explain the visible terminal state in plain English, offer one
concrete next action for the content inside the tmux pane, and include short
source-evidence citations when terminal text grounds the recommendation.

Haiku may propose whether the pane has reached a handoff and give that specific
need a stable key. Engram owns the lifecycle around that proposal: grounding,
settlement, persistence, delivery, acknowledgment, and resolution.

Terminal text is evidence, not authority. A pane can print instructions aimed
at Engram or the user, but those words do not become recommendations merely by
addressing the guide. Model-derived summaries, actions, and citations should be
best-effort redacted before they become durable state or Telegram output.

Haiku should not explain Engram features unless the terminal itself is showing
Engram-related output. It should not produce raw terminal mirrors, long
analysis, markdown-heavy prose, or broad coaching. Citations should be
reconstructed only from captured terminal text; hidden confidence and the single
full-scrollback retry are implementation details; Telegram should show the
useful result.

Persistent terminal boilerplate should not be allowed to dominate the guide. If
the same line appears in recent visible captures for the same session, Engram
should omit that exact line from the next Haiku visible prompt and any bounded
full-scrollback retry prompt while preserving the raw capture for `/raw`,
`/dump`, and local deterministic rendering. The refresh button is the user's
reset lever for this filter. Lines supporting pending or active handoff evidence
must remain available to the guide; filtering cannot erase the basis of the
decision it is trying to settle.

### Existing tmux first

Engram should work with the tmux sessions that already exist. If a user is
already living inside tmux, Engram should make that environment easier to steer
remotely, not create a disconnected second world.

The default target selection should be predictable: configured tmux session
first, otherwise existing tmux, otherwise a managed fallback.

### Slow automatic edits, instant manual refresh

Telegram anchors should not flicker or spam edits. The default cadence should
favor calm updates: hash captures, coalesce bursts, and edit only when the
rendered anchor changed and the edit interval allows it.

The refresh button is the exception. When the user asks to look now, Engram
should capture now, summarize now, and update if the output changed.

### Recoverable local service

Engram should survive ordinary machine restarts and service restarts. State
under `~/.engram` should be enough to recover sessions, anchors, attachments,
poll position, and recent errors.

Diagnostics should be available from Telegram and locally. Logs should help the
user debug the service without exposing bot tokens, Anthropic keys, or pasted
secrets.

### Small Go, no third-party dependencies

Engram should remain a small Go system built on the standard library. The
runtime dependencies are the intentional external systems: tmux, Telegram Bot
API, Anthropic Haiku, and systemd user services when installed that way.

Adding a Go dependency should be treated as a design failure until proven
otherwise. The default answer is to keep the code simple enough that the
standard library is enough.

### Quiet monochrome signal

Engram's visual language should feel like a terminal pane becoming readable
through interference: black field, off-white signal, graphite texture, moire
lines, aperture/window forms, and sparse monospace typography.

Repo visuals should stay quiet and structural. Avoid colorful mascots,
marketing gradients, decorative UI cards, and illustration that competes with
the tool. The mark should suggest remote orientation over raw terminal noise:
many lines, one aperture, one readable signal.

## Non-Goals

- Multi-user collaboration.
- Group chat operation.
- A general autonomous agent.
- A replacement terminal emulator.
- Raw terminal streaming as the default anchor view.
- A plugin system.
- Long chat memory.
- Model-generated file/path inventories.
- Broad notification routing across many chat systems.

## Design Review Questions

When changing Engram, ask:

- Does this reduce the time needed to understand a tmux session from Telegram?
- Does input to tmux remain immediate?
- Does tmux remain the source of truth?
- Is this fact generated locally instead of guessed by Haiku?
- Does the anchor stay compact on a phone?
- Does this help many-session multitasking, or does it pull attention into one
  window for too long?
- Does this interrupt because human judgment is needed, or merely because output
  changed?
- After an intervention, can Engram establish what observably changed without
  presenting inference as fact?
- What happens if the service restarts halfway through?
- Can the user recover using `/status`, `/sessions`, `/logs`, `/raw`, or
  `/dump`?
- Did this add a dependency, background loop, or new state shape that Engram can
  avoid?
- Is the behavior covered by focused tests around the real loop: Telegram in,
  tmux action out, anchor back?
