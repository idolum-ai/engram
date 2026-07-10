# Security Requirements

Engram intentionally bridges Telegram, local tmux, the local filesystem, and
one selected presentation dependency: Anthropic or Chromium. The security and
privacy model must stay small and explicit.

## Identity

- Exactly one Telegram user is authorized.
- Exactly one Telegram chat is authorized.
- DM-only operation is supported; group operation is out of scope.
- Unauthorized messages and callbacks must not mutate tmux, sessions,
  attachments, or processed-message state. Poll offsets and a generic bounded
  rejection record may advance so rejected updates are not replayed.

## Secrets

- `.env` files must not be tracked.
- Runtime env files must be regular files with no group or other permissions.
- Bot tokens and Anthropic keys must not appear in tracked files, diagnostics,
  issues, or test fixtures.
- Audit payloads and `/logs` output must redact configured credentials and
  common credential patterns.
- Model-derived summaries, recommended actions, and handoff evidence must pass
  through the same best-effort redaction before persistence and Telegram
  delivery.
- Documentation must state that redaction is best effort and does not make an
  artifact safe to share without review.

## External Data Flow

- Documentation must explain that Telegram receives commands, replies,
  summaries, captures, logs, and files sent through bot commands.
- Documentation must explain that Anthropic is used only in `guide` mode and
  receives session metadata, input previews, prior summaries, visible pane
  captures, and an optional bounded full-scrollback retry.
- Terminal captures sent to Anthropic are not credential-redacted.
- Terminal captures are untrusted data for the guide. Pane-authored text cannot
  instruct Haiku or acquire authority merely by addressing Engram or the user.
- Incoming attachments are downloaded from Telegram but are not sent to
  Anthropic by default.
- Terminal image snapshots are exact, unredacted transcript data. They are sent
  to a local headless browser and then to the configured Telegram DM, never to
  Anthropic. Terminal text must be HTML-escaped; browser networking, extensions,
  and persistent profiles must be disabled.
- In `snapshot` mode, exact terminal images are sent automatically whenever a
  changed live anchor is rendered, not only after an explicit image request.
- `snapshot` mode must not initialize or call an Anthropic client even when an
  Anthropic credential remains in the env file.

## Local Sensitive Data

- `state.json` may contain Telegram identifiers, input previews, summaries,
  hashes, attachment metadata, and pending or active handoff text and evidence.
  Raw terminal captures are retained only in process memory and are omitted from
  persisted state.
- `audit.jsonl`, lock metadata, tmux history, and `/tmp/engram` artifacts must
  be treated as sensitive.
- Audit storage retains only a bounded current file and one bounded predecessor.
  Unauthorized audit and update-journal records must not retain the rejected
  sender's user or chat identifiers.
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
- Snapshot HTML, isolated browser profiles, and PNGs must use private temporary
  paths and be removed after upload or failure.

## Process Ownership

- A lock prevents two Engram instances from polling the same Telegram settings.
- Service restart should preserve tmux sessions and state.

## Vulnerability Handling

- The current code line and latest tagged release, when available, receive
  security updates; older versions are unsupported.
- Public documentation must link to the repository's private GitHub security
  advisory reporting route and tell reporters not to disclose secrets or
  transcript data publicly.
