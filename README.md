# Engram

Engram is a Telegram client for tmux sessions. It creates tmux-backed terminal
sessions from Telegram messages, keeps one editable Telegram anchor per session,
and uses Anthropic Haiku to render a compact, faithful summary of the visible
terminal state.

## Requirements

- Go
- tmux
- systemd user services for `make install-service`
- A Telegram bot token
- An Anthropic API key

Engram uses only the Go standard library.

## Configure

Create `~/.engram/.env`:

```sh
mkdir -p ~/.engram
cp .env.example ~/.engram/.env
```

Fill in:

```sh
TELEGRAM_BOT_TOKEN=
TELEGRAM_ALLOWED_USER_ID=
ANTHROPIC_API_KEY=
```

Engram accepts exactly one Telegram user. For DMs, leave `TELEGRAM_CHAT_ID`
blank and Engram will use `TELEGRAM_ALLOWED_USER_ID` as the private chat ID.

## Run

```sh
make run
```

or:

```sh
go run ./cmd/engram run --env ~/.engram/.env
```

## Install

```sh
make install PREFIX="$HOME/.local"
```

Install and start the systemd user service:

```sh
make install-service PREFIX="$HOME/.local"
```

For boot without an interactive login, enable lingering manually:

```sh
loginctl enable-linger "$USER"
```

## Telegram Commands

Use `/help` in Telegram for the command list. Use `/commands` to receive the
machine-readable command metadata as JSON, or run `engram commands` locally.
Common commands:

- `/sessions`
- `/new <text>`
- `/send <id> <text>`
- `/key <id> <keys...>`
- `/dump <id>`
- `/raw <id>`
- `/close <id>`
- `/attachments`
- `/download <absolute-path>`
- `/logs`
- `/status`
- `/version`
