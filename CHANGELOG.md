# Changelog

Notable user-visible and operational changes are recorded here.

## Unreleased

### Conversational guide

- Add a disabled-by-default Codex historical-context path. Exact pane-local
  UUID and process-incarnation proof can supply a bounded number of recent
  visible user/assistant messages to the guide without making them current
  terminal truth. Future sessions bind through the `SessionStart` hook;
  existing active sessions can run the argument-free `engram codex-bind`
  command without restarting. Long-lived rollouts use bounded prefix identity
  and recent-tail reads. Foreground-process-group and precise kernel-start proof
  prevent stale or background runtimes from supplying context; provider-session
  bindings are revalidated through publication. Full messages are redacted
  before truncation. A deterministic, Unicode-width-aware detector may render
  one exactly cropped box/arrow diagram as a separately labeled Codex-context
  inset; literal snapshots and raw captures remain unchanged.

### GitHub capabilities

- Let one encrypted GitHub App alias enroll a repeatable, authenticated set of
  installation IDs while keeping every request, approval, grant, lease, token,
  status line, and audit event bound to exactly one selected installation.
  Multi-installation requests require an explicit `--installation-id`; Engram
  refuses ambiguous, unknown, or cross-installation authority instead of
  guessing or combining tokens. Existing single-installation vaults remain
  readable without an automatic secret rewrite.
- Validate every requested repository against a selected-repository
  installation before storing renewable authority. A same-owner repository
  that is not installed now fails during grant creation instead of creating an
  unusable grant that fails only when first consumed.
- Add bounded renewable, pane-scoped work-session grants. One explicit
  Telegram approval can authorize subset commands for a configured maximum of
  eight hours while Engram rotates ordinary short-lived GitHub installation
  tokens in memory. Grants fail closed on pane, watch, enrollment, process,
  expiry, or explicit-revocation changes and remain visibly distinct from the
  current token lease. The approved expiry is immutable, token delivery is
  committed transactionally, concurrent subset children share one token at the
  displayed ceiling, and renewable writes use an explicit collaboration
  allowlist.
- Compatibility: the local GitHub broker protocol is now version 3 so mixed
  old/new CLI and daemon processes fail closed instead of exposing a
  provisional token during transactional delivery or silently dropping the
  installation selector. Upgrade the CLI and daemon together.
- Compatibility: `engram github status --json` now emits an object with
  `grants` and `leases` arrays instead of the previous bare lease array. Update
  consumers to read the `leases` field.

## [v0.7.0] - 2026-07-27

### Attention

- Collapse any number of quiet session anchors into one pinned, recoverable
  shelf. Hidden sessions remain tracked without accepting stale replies or
  occupying one pinned Telegram message each, and `Show` restores their current
  anchors together.
- Add a confirmed natural-language key composer for installations with a guide
  provider. Engram translates a plain-language request into a closed set of
  tmux keys, shows the exact destination and sequence, and sends no keys to
  tmux until the authorized user confirms it.

### Conversational guide

- Use visible terminal structure to distinguish prompts, output, activity,
  model identity, and interface chrome. Version-specific adapters cover Codex
  CLI `0.144.5` and `0.144.6`, and Claude Code `2.1.219` on Linux; `2.1.206` is
  retained for the hermetic fixture. Unsupported versions and uncertain
  layouts use the generic semantic guide, while raw captures, snapshots, and
  references remain unchanged.
- For supported Claude layouts, retain visibly established model, effort, and
  activity while the same process remains running, and omit recognized
  composer, status, spinner, and token-saving chrome from guide evidence.

### Terminal images

- Let foreground Telegram input preempt queued or active read-only tmux
  captures used by background guide and snapshot refreshes, reducing delays
  before interactive keys and replies reach the pane.
- Recover Chromium snapshot rendering after transient browser failures by
  probing health and re-establishing the renderer without requiring a service
  restart.
- Render alternate-screen snapshots at their captured viewport height instead
  of placing short Claude-style frames on a synthetic 64-row canvas.

### GitHub capabilities

- Add an optional pane-scoped GitHub App broker. After Telegram approval,
  Engram launches the requested child command with a short-lived installation
  token limited to named repositories and permissions; the App private key
  remains in an encrypted local vault. The approved child remains trusted to
  use or disclose its token.
- Bind in-memory leases to the watched tmux server, window, pane, and complete
  enrollment identity, and revalidate those bindings immediately before
  returning a reused or newly minted token. Pane or enrollment replacement,
  removal, retargeting, or revocation invalidates the affected lease; an
  enrollment change while minting discards and revokes the new token.

### Security

- Isolate the GitHub capability broker socket per Engram home and Telegram
  identity, refuse to displace a live listener, and preserve replacement
  sockets when an older broker closes.
- Enforce owner-only, non-symlink source PEM files during enrollment. A corrupt
  optional GitHub App vault now disables that capability visibly without
  preventing Telegram and tmux service startup.
- Erase locally supplied GitHub broker passphrase buffers on every request
  return path, including unavailable capability, rejected binding, and
  successful lease reuse.

### Compatibility

- State migrates from schema 15 to schema 17 on its first save. Back up
  `~/.engram/state.json` before upgrading when rollback matters: after
  migration, v0.6.0 and older binaries reject the newer schema.

### Verification

- Add hermetic semantic-screen fixtures, snapshot recovery tests, tmux input
  priority coverage, collapsed-shelf lifecycle tests, and adversarial key
  composer cases.
- Add sanitized Claude Code `2.1.219` replays and deterministic coverage for
  model-card loss, activity, approval, model switching, process replacement,
  restart continuity, unsupported versions, and semantic lookalikes.

### Fixed

- Omit absent Telegram reply markup instead of serializing an empty keyboard,
  restoring ordinary transcription replies and other messages without
  controls.

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
