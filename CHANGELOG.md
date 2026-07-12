# Changelog

Notable user-visible and operational changes are recorded here.

## Unreleased

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
