# Security

Engram bridges Telegram and local tmux panes. Treat the Telegram bot token as
shell access to the configured account.

## Supported Scope

- One configured Telegram user.
- One configured Telegram chat.
- Local tmux and local filesystem access on the host running Engram.

## Reporting

Open a private report through the repository owner before disclosing issues that
could expose credentials, send unauthorized tmux input, or upload local files.

## Operational Guidance

- Keep `~/.engram/.env` mode `0600`.
- Do not track `.env`, logs, state files, PEM files, or downloaded attachments.
- Run `make secrets` before pushing.
- Prefer DM-only operation unless group support is explicitly tested.
