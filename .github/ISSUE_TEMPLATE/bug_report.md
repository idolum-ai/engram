---
name: Bug report
about: Report incorrect Engram behavior
labels: bug
---

## What Happened

Describe the failure and the command or workflow that triggered it.

## Expected Behavior

## Reproduction Steps

1.
2.
3.

## Environment

- Engram version or commit (`engram version`):
- OS and version:
- Go version (`go version`):
- tmux version (`tmux -V`):
- Install mode (`make run`, installed binary, Linux systemd, or macOS manual):
- Telegram chat type (supported reports must say `private` / direct message):

## Diagnostics

- Local `engram status --env ~/.engram/.env` result:
- For Linux systemd, relevant `systemctl --user status engram.service` result:
- Minimal relevant journal or `/logs` lines:

Redact before posting. Logs, state, terminal output, paths, and Telegram messages
may contain credentials or private transcript data. Engram's automatic
redaction is pattern-based and may miss secrets. Never attach `.env`,
`state.json`, raw `/dump` or `/raw` output, bot tokens, API keys, or unrelated
terminal history.

For a possible security issue, stop and use the private route in `SECURITY.md`.
