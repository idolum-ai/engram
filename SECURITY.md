# Security Policy

Engram bridges Telegram and local tmux panes. Control of the authorized
Telegram account can become shell access to the local account running Engram.
A stolen bot token can expose bot traffic, impersonate bot replies, or disrupt
polling and must be revoked immediately.

## Supported Versions

Engram is pre-release software. Security fixes are made on the current code
line and are not backported broadly.

| Version | Security updates |
| --- | --- |
| Current `main` | Yes |
| Latest tagged release, when available | Yes |
| Older releases, commits, and forks | No |

## Report A Vulnerability

Use GitHub's private
[Report a vulnerability](https://github.com/idolum-ai/engram/security/advisories/new)
route. Include the affected version or commit, impact, reproduction steps, and
any proposed mitigation. Do not open a public issue for a suspected
vulnerability and do not include live bot tokens, API keys, private terminal
transcripts, or downloaded files in the report. Use synthetic values and the
smallest redacted evidence that demonstrates the problem.

Reports about unauthorized tmux input, authorization bypass, credential
exposure, unsafe local file upload/download, attachment handling, or redaction
failure are in scope.

## Security Boundary

- Exactly one Telegram user and one chat are authorized. DM-only operation is
  the supported deployment.
- Authorized Telegram input can execute shell commands and key presses in tmux.
- `/download` can upload a chosen local regular file to Telegram. `/raw` and
  `/dump` upload terminal content.
- Bounded visible pane captures may be sent to the selected conversational
  provider, Anthropic Haiku or OpenAI Luna. Replied Telegram voice notes default
  to retained local attachments whose paths are delivered as literal tmux
  input. Only explicit `VOICE_INPUT_MODE=transcribe` sends them to OpenAI
  `gpt-4o-transcribe` and delivers a bounded transcript instead.
- State, logs, generated captures, and attachments remain on the local host and
  may contain sensitive transcript data.

## Operational Guidance

- Keep `~/.engram/.env` mode `0600` and its parent directory private.
- Use a dedicated Telegram bot in a direct message. Do not add it to groups.
- Revoke and replace the bot token, Anthropic key, or OpenAI key immediately if
  exposed.
- Do not track `.env` files, state, logs, PEM files, generated captures, or
  downloaded attachments.
- Treat Engram's private runtime root as sensitive. Engram prefers a valid
  private `XDG_RUNTIME_DIR` and otherwise uses a UID-specific directory under
  the system temporary directory.
- Review exact paths before using `/download` and review every artifact before
  sharing it.
- Run `make secrets` before pushing.

Audit and `/logs` redaction is pattern-based. It can miss unknown credential
formats and sensitive prose, and it does not redact terminal captures,
`state.json`, `/raw`, `/dump`, `/download`, attachments, Telegram history, or
model-provider requests.
