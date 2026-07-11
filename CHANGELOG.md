# Changelog

Notable user-visible and operational changes are recorded here.

## Unreleased

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

### Distribution

- Add a reviewed release pipeline with versioned Linux and Darwin archives,
  SHA-256 checksums, candidate evidence, and a checksum-verifying installer.
