# Terminal Mechanics Boundary

Status: design proposal; no runtime behavior is committed by this document.

## Decision

Keep Engram a Telegram control surface for tmux. Extract only the mechanics that
are intrinsically about tmux into a private package boundary, without defining a
frontend framework or promising another product surface.

Telegram remains where Engram becomes useful: a phone-sized map of many remote
panes, one current pinned anchor per watch, fast replies, attachments, and quiet
attention. The extraction exists to make those mechanics easier to test and
harder to accidentally couple to delivery. It is not a step toward replacing
Telegram with a local terminal application.

The proposed shape is:

```text
Telegram orchestration -> private terminal mechanics -> tmux
                       -> presentation (Haiku/Chromium)
                       -> Telegram delivery and anchors
```

No public interface, daemon, socket, HTTP listener, filesystem inbox, plugin
host, generic transport, TUI, or MCP server follows from this decision.

## Why This Boundary Is Earned

Engram already performs a small body of work whose truth does not come from
Telegram:

- bind an Engram watch to immutable tmux pane and window identity;
- revalidate that identity immediately before a pane-bound effect;
- distinguish Engram-created windows from attached existing windows;
- capture one bounded physical and logical terminal frame;
- send typed command, literal text, and validated key input;
- observe bounded, untrusted terminal attention records.

These rules should be testable without constructing a Telegram client. Today,
Telegram orchestration is still their only product caller. That is acceptable.
A private boundary may have one caller when it isolates a real source of truth,
failure mode, and test seam.

The boundary has failed if it merely renames existing application methods,
requires Telegram-shaped placeholders, or introduces abstractions for imagined
consumers.

## What Stays With Telegram

Telegram owns every concept whose meaning depends on durable chat messages:

- user and chat admission, update polling, offsets, and deduplication;
- canonical pinned anchors and alternate conversational or image messages;
- current-versus-stale reply routing and message generations;
- callbacks, inline keyboards, reply lookup, and polite stale errors;
- message replacement, compaction, pin reconciliation, and delivery retries;
- incoming attachments, outgoing documents, and download bypasses;
- Telegram-specific audit references and recovery state.

The mechanics package must not invent generic views, routes, principals,
deliveries, pins, media, or callbacks. If another phone-capable carrier is ever
proposed, its overlap with Telegram should be learned from working behavior
rather than predicted here.

## Private Mechanics

A plausible private package owns:

- immutable `%pane_id` and `@window_id` bindings;
- watch provenance and pane lifecycle validation;
- existing-session selection and attach/new distinctions;
- bounded `CaptureStyled` frames with shared physical/logical coordinates;
- command-plus-Enter, literal-text, and allowlisted-key operations;
- cwd and rename operations whose truth comes from tmux;
- attention-record parsing, validation, and deduplication.

It returns bounded domain values and typed failures. It does not receive a
generic `Execute(string)` callback, arbitrary tmux target, Telegram credential,
delivery identifier, or renderer.

Scheduling belongs where its reason lives. Capture coalescing shared by guide
and snapshot presentation may remain application orchestration until extraction
clearly simplifies it. Telegram edit cadence and retry policy remain Telegram
concerns.

## Invariants

1. tmux remains the workspace and source of terminal truth.
2. Every pane-bound effect revalidates immutable pane and window identity.
3. Names and indexes are for display and selection, never effect-time authority.
4. Input remains independent of Telegram delivery, Haiku, and Chromium.
5. One bounded capture supplies physical ANSI and joined logical text over the
   same coordinates.
6. Created and attached windows retain different close semantics.
7. Terminal attention records remain bounded, best effort, deduplicated, and
   untrusted; they never become commands or identity.
8. Pane loss is reported only from conclusive tmux evidence.
9. Go remains standard-library-only.
10. The extraction does not change observable Telegram behavior.

## Local Read Probe

After the boundary exists, a tiny read-only command may prove that terminal
mechanics are genuinely independent of Telegram construction. Its maximum
surface is:

- local status of tmux availability and Engram state readability;
- tracked watches with immutable identity and provenance;
- one bounded literal frame selected by an Engram watch ID.

The probe opens no listener, performs no network request, starts no background
worker, renders no ANSI control bytes, mutates neither tmux nor state, and
requires no Telegram, Anthropic, or Chromium configuration. It is a diagnostic
and architecture test, not a second Engram interface.

If a useful probe requires generic routing, frontend state, mutation, or a
resident process, do not build it.

## Rejected Directions

- **Local TUI:** tmux already provides the better local terminal interface. A
  second pane manager would add a surface without creating Engram's remote phase
  change.
- **MCP:** exporting pane context to an agent host introduces a large trust and
  retention boundary without improving the direct phone-to-tmux loop.
- **Generic frontend or transport interface:** there is no second real carrier
  from which to learn the common shape.
- **Unix socket, HTTP, or filesystem inbox:** each creates ambient command
  authority, lifecycle, and failure semantics Engram does not need.
- **Simultaneous control surfaces:** competing notions of the current actionable
  representation would weaken the one-anchor discipline.
- **Dynamic adapters or plugins:** Engram is a precise product, not an ecosystem.

These are rejected by this proposal, not reserved as implementation stages.
Reconsidering one requires a new user need and design decision.

## Telegram Optionality

This proposal does not claim that Telegram is optional. Removing a required
credential while leaving only a local diagnostic would be technically true and
productly misleading: local tmux already exists.

Telegram can become optional only when a second concrete, phone-capable carrier
demonstrates the same Engram-shaped value: low-dwell orientation across many
remote panes, immediate typed input, one current actionable representation, and
recoverable delivery. That proposal should begin with the carrier and the user
experience, then extract only overlap proven by both implementations.

Until then, Telegram is a productive constraint and the reference product
surface. Terminal mechanics are private implementation truth, not a platform.

## Decision Test

Before extracting any operation, ask:

- Is its truth determined by tmux rather than Telegram presentation?
- Does isolating it make identity, input, or failure behavior easier to prove?
- Can Telegram behavior remain byte-for-byte or observably compatible?
- Is the API typed, bounded, private, and smaller than the code it replaces?
- Would deleting the boundary make Engram clearer? If yes, delete it.
