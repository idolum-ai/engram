# Security Requirements

Engram intentionally bridges Telegram and a local shell. The security model is
small and explicit.

## Identity

- Exactly one Telegram user is authorized.
- Exactly one Telegram chat is authorized.
- Group chats are not required for the default deployment.

## Secrets

- `.env` files must not be tracked.
- Bot tokens and Anthropic keys must not appear in tracked files.
- `/logs` must redact Telegram and Anthropic credentials.

## Local Effects

- Telegram messages can cause shell input in tmux.
- Attachments are saved under `/tmp/engram/attachments`.
- Large attachments require a hash-confirmed bypass.
- `/download` only accepts absolute paths and uploads regular files.

## Process Ownership

- A lock prevents two Engram instances from polling the same Telegram settings.
- systemd restart should preserve tmux sessions and state.
