<p align="center">
  <img src="docs/assets/engram-mark.svg" alt="Engram: a monochrome moire aperture over a dark terminal field" width="760">
</p>

<h1 align="center">Engram</h1>

<p align="center">
  <strong>Remote tmux, rendered as a quiet signal.</strong>
</p>

Engram is a Telegram client for tmux sessions. It creates tmux-backed terminal
sessions from Telegram messages, keeps one editable Telegram anchor per session,
and uses Anthropic Haiku to render a compact plain-English status report with a
recommended next action plus short source-evidence quote blocks when useful.

## Requirements

- Go
- tmux
- systemd user services for `make install-service`
- A Telegram bot token
- An Anthropic API key

Engram uses only the Go standard library.

## Quality Gate

Run the local quality gate before pushing:

```sh
make check
```

The gate runs build/tests, package-boundary checks, public-readiness checks,
workflow sanity checks, a stdlib-only check, docs freshness checks, and a
tracked-file secret scan.

For a feature-by-feature readiness view, see
[`docs/feature-matrix.md`](docs/feature-matrix.md).
For the product and implementation principles behind those features, see
[`docs/design-principles.md`](docs/design-principles.md).

## Configure

Create `~/.engram/.env`:

```sh
mkdir -p ~/.engram
install -m 0600 .env.example ~/.engram/.env
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
- `/attach <tmux-target>`
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

Local service checks:

- `engram preflight --env ~/.engram/.env`
- `engram status --env ~/.engram/.env`
- `engram dry-start --env ~/.engram/.env`

`/sessions` shows both Engram-tracked terminal sessions and native tmux sessions.
By default, new Engram windows are created in an existing tmux session when one
is available. Set `ENGRAM_TMUX_SESSION` to force a specific session name.
Use `/attach <target>` with a target shown under `/sessions`, such as
`/attach 0:1`, to track an existing tmux window as an Engram session.
The `/sessions` response also includes attach buttons for untracked windows.
