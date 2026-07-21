# Changelog

Notable user-visible and operational changes are recorded here.

## Unreleased

## [v0.6.0] - 2026-07-21

### Recovery

- Persist a bounded, redacted per-window recovery ledger, accept exact Codex
  session metadata through an opt-in `SessionStart` hook, and detect host/tmux
  incarnation loss at startup.
- Add deterministic `/recovery` plans and compact Telegram controls that resume
  exact provider sessions while keeping observed shell launches advisory and
  never replaying arbitrary commands automatically.

### Input

- Add explicit one-pass `{engram:name}` input templates with `/remember`,
  `/forget`, and `/templates export`. Expansion uses the existing guarded tmux
  routes, never recurses, never learns from history, and never triggers from
  terminal output.
- Persist exact user-authored template bodies in a private `templates.json`,
  guard the complete Engram home with one process lock, and expose the full
  template set only through an explicit authorized export.

### Terminal images

- Preserve fitting physical terminal rows in compact guide evidence through a
  96-column mobile readability limit, with disclosed wrapping only for wider
  rows and context bounded by terminal block boundaries.
- Supervise snapshot browser process groups and bound inherited output pipes so
  a timed-out or completed wrapper cannot leave descendants monopolizing the
  global render slots.

### Configuration

- Safely tighten an existing owner-controlled `ENGRAM_HOME` to mode `0700`,
  preserve recursive creation for nested custom paths, and accept canonical
  macOS parent aliases while continuing to reject an unsafe home leaf.

### Verification

- Make real-tmux integration independent of operator tmux configuration and
  canonical Darwin temporary paths, and require the release candidate to run
  that suite natively on both Linux and macOS.

## [v0.5.0] - 2026-07-18

### Configuration

- Add an optional trusted local snapshot-status command whose sanitized,
  bounded one-line output appears in image footers. Engram owns the layout
  budget, runs the command only during an existing render with a short timeout
  and secret-free environment, and never lets status-only changes trigger
  automatic Telegram edits.

### Conversational guide

- Prefer durable outcomes and current work over terminal narration, routine
  mechanism, idle prompts, and unexecuted interface text, guided by human
  preference fixtures and a reproducible prompt tournament.

### Input

- Add a distinct `← ↑ ↓ →` row to current snapshot anchors for direct tmux
  directional input under the existing authenticated callback boundary.
- Send long and multiline Telegram replies as one bracketed tmux paste followed
  by one Enter, allowing terminal applications to receive the complete input as
  one submission.
- Add voice replies with explicit startup modes: local durable attachment-path
  delivery by default, or opt-in `gpt-4o-transcribe`. Transcription audio is
  temporary, transcript provenance is explicit, and latest-view plus
  immutable-pane checks are repeated before either input reaches tmux.
- Recognize versioned, length-framed upstream-signal records through bounded
  presentation indent and same-indent wrapping so Codex-rendered command output
  can request attention without consuming adjacent guide evidence.

### File handoff

- Show only existing regular files in anchor reference sections, enumerate and
  code-format them in both modes, and add matching `⬇️ n` buttons that reuse
  `/download` behind current-card and exact-list callback guards.

### Verification

- Add a manually dispatched hermetic golden path that drives the compiled
  service through a local Telegram simulator, isolated real tmux, and real
  Chromium, retaining reviewable snapshot and interaction evidence without
  repository secrets.

### Fixed

- Avoid silently launching desktop Chrome or Chromium for snapshot rendering on
  macOS. Automatic discovery now requires a dedicated headless executable there,
  while Linux retains its existing browser fallbacks and explicit configuration
  remains available on both platforms.
- Use current Telegram `reply_parameters` and `link_preview_options` payloads
  for outgoing text and snapshot replies, with the hermetic simulator enforcing
  reply identity against known messages.
- Recognize the missing-server diagnostic emitted by tmux 3.3a on a clean
  socket root, allowing Engram to create its configured tmux session instead
  of rejecting the first new window.

## [v0.4.0] - 2026-07-14

### Conversational guide

- Continue guide renderings naturally across strongly aligned captures while
  keeping the complete current terminal frame as the sole factual authority.
  Continuity remains process-local, isolated per window, and discarded across
  program, pane, mode, refresh, reattachment, or service-restart boundaries.
- Keep placeholder prompts, model-status footers, and upstream signal records
  out of conversational evidence without changing screenshots, raw captures,
  references, or capture hashes.

### Configuration

- Add OpenAI Luna as an opt-in conversational guide selected with
  `LLM_PROVIDER=openai`, `OPENAI_API_KEY`, and `OPENAI_MODEL=gpt-5.6-luna`.
  Anthropic Haiku 4.5 remains the default and existing configuration stays
  compatible.
- Give Haiku and Luna the same bounded evidence, prompt, non-streaming request,
  and deterministic 180-word response limit. Provider changes require a service
  restart and only the selected provider receives terminal evidence.
- Report the selected guide provider and model in diagnostics and `/status`,
  and extend audit and presentation redaction to configured OpenAI credentials
  and common OpenAI key shapes.

## [v0.3.0] - 2026-07-13

### Fixed

- Treat numeric tmux session names as sessions when creating windows, so `/new`
  no longer fails by interpreting a name such as `0` as an occupied window
  index.
- Preserve immutable session, window, and pane IDs when attaching to real tmux
  targets by replacing delimiter-based metadata parsing with strict
  byte-length-framed records. This makes tmux 3.2 or newer an explicit runtime
  requirement.
- Guard each text and key effect inside tmux with the persisted server/window
  identity so a server restart cannot redirect input to a reused pane ID.

## [v0.2.0] - 2026-07-12

### Configuration

- Add `TELEGRAM_API_BASE` for routing Bot API methods and file downloads through
  a configurable HTTP(S) Telegram API server root.

## [v0.1.0] - 2026-07-12

### Product

- Operate existing or Engram-created tmux windows from one authorized Telegram
  DM, with latest-only reply routing and stable pinned session anchors.
- Choose conversational Anthropic Haiku summaries or exact Chromium-rendered
  terminal images, with on-demand access to either capability when configured.
- Transfer bounded attachments, raw panes, scrollback, local files, and visible
  paths or URLs without adding a separate remote-control protocol.
- Send terminal-native upstream signals from nested containers or child
  environments without sharing Telegram credentials.

### Safety and operations

- Restrict admission to one configured Telegram user and private chat, lock one
  poller per credential tuple, validate immutable tmux pane identity, and retain
  bounded local state and redacted audit logs.
- Support Linux systemd user services and foreground macOS operation. Haiku and
  Chromium remain optional, with their data boundaries documented separately.
- Add read-only `engram inspect` diagnostics for local status, tracked watches,
  and bounded literal frames without Telegram configuration or network access.
- Isolate pane-bound identity, input, capture, scrollback, and close operations
  behind a private tmux mechanics boundary while keeping Telegram anchors and
  routing in the application layer.
- Bind watches to a tmux server incarnation, make destructive close atomic with
  identity validation, and require explicit reattachment after legacy state or
  a server restart.
- Use a private per-user runtime root for attachments and generated artifacts,
  with owner, mode, symlink, and exclusive-creation checks.

### Distribution

- Add a reviewed release pipeline with versioned Linux and Darwin archives,
  SHA-256 checksums, candidate evidence, and a checksum-verifying installer.
