# Security Requirements

Engram intentionally bridges Telegram, Anthropic, local tmux, and the local
filesystem. The security and privacy model must stay small and explicit.

## Identity

- Exactly one Telegram user is authorized.
- Exactly one Telegram chat is authorized.
- DM-only operation is supported; group operation is out of scope.
- Unauthorized messages and callbacks must not mutate tmux or state.

## Secrets

- `.env` files must not be tracked.
- Runtime env files must be regular files with no group or other permissions.
- Bot tokens and Anthropic keys must not appear in tracked files, diagnostics,
  issues, or test fixtures.
- Audit payloads and `/logs` output must redact configured credentials and
  common credential patterns.
- Documentation must state that redaction is best effort and does not make an
  artifact safe to share without review.

## External Data Flow

- Documentation must explain that Telegram receives commands, replies,
  summaries, captures, logs, and files sent through bot commands.
- Documentation must explain that Anthropic receives session metadata, input
  previews, prior summaries, visible pane captures, and an optional bounded
  full-scrollback retry.
- Terminal captures sent to Anthropic are not credential-redacted.
- Incoming attachments are downloaded from Telegram but are not sent to
  Anthropic by default.

## Local Sensitive Data

- `state.json` may contain Telegram identifiers, input previews, summaries,
  hashes, and attachment metadata. Raw terminal captures are retained only in
  process memory and are omitted from persisted state.
- `audit.jsonl`, lock metadata, tmux history, and `/tmp/engram` artifacts must
  be treated as sensitive.
- Uninstall must not silently destroy local state or tmux sessions.

## Local Effects

- Telegram messages can cause shell input in tmux.
- Attachments are saved under `/tmp/engram/attachments`.
- Large attachments require a hash-confirmed bypass and remain subject to a
  hard limit derived from the configured soft limit, free disk, and Telegram's
  20 MiB cloud Bot API download ceiling.
- `/attachment_bypass` is the registered large-attachment authorization
  command.
- Attachment soft limits must be enforced during the download stream, not only
  from Telegram-provided file metadata.
- `/download` only accepts absolute paths and uploads regular files.
- `/download` rejects symlinks and non-regular files.
- `/download` uploads from a private bounded snapshot of the already-opened
  source so path replacement cannot redirect a queued transfer.
- The private snapshot name must not leak into Telegram. `/download` preserves
  the original source basename as the Telegram-visible document filename.
- `/download` rejects files above Telegram's 50 MiB cloud Bot API multipart
  upload ceiling before opening a network request.
- Attachment downloads hash while streaming, and long file transfers run in
  bounded background workers and a bounded queue so polling remains responsive.
- Generated `/raw` and `/dump` artifacts must not exceed the same 50 MiB cloud
  upload ceiling.

## Process Ownership

- A lock prevents two Engram instances from polling the same Telegram settings.
- Service restart should preserve tmux sessions and state.

## Vulnerability Handling

- The current code line and latest tagged release, when available, receive
  security updates; older versions are unsupported.
- Public documentation must link to the repository's private GitHub security
  advisory reporting route and tell reporters not to disclose secrets or
  transcript data publicly.
