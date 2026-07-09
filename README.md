<p align="center">
  <img src="docs/assets/engram-mark.svg" alt="Engram: a monochrome moire aperture over a dark terminal field" width="760">
</p>

<h1 align="center">Engram</h1>

<p align="center">
  <strong>Remote tmux, rendered as a quiet signal.</strong>
</p>

Engram is a single-user Telegram control surface for local tmux sessions. It
creates or attaches to tmux windows, routes Telegram messages into panes, and
uses Anthropic Haiku to turn terminal captures into compact status anchors.

## First Run

### 1. Install prerequisites

You need:

- Linux or macOS
- Go 1.22 or newer
- tmux, Git, Make, and curl
- A Telegram account
- An Anthropic API key with access to Claude Haiku 4.5

Linux with a systemd user session is the supported service installation. macOS
is compile-checked and runs manually in the foreground; Engram does not install
a launchd service.

Clone and enter the repository:

```sh
git clone https://github.com/idolum-ai/engram.git
cd engram
```

### 2. Create the Telegram bot

1. Open the verified `@BotFather` account in Telegram.
2. Send `/newbot` and follow its prompts.
3. Keep the returned token private. It controls the bot.
4. Open a direct message with the new bot and send `/start`.

Before Engram starts polling, retrieve that DM from the official Telegram Bot
API. This keeps the token out of shell history and the `curl` argument list:

```bash
read -rsp "Bot token: " BOT_TOKEN; printf '\n'
printf 'url = "https://api.telegram.org/bot%s/getUpdates"\n' "$BOT_TOKEN" \
  | curl --silent --show-error --config -
unset BOT_TOKEN
```

In the JSON response, use the integer at `message.from.id` for the update whose
`message.chat.type` is `private`. Do not use `update_id` or the bot's own ID.
The response also contains your DM text, so do not paste it into an issue.

### 3. Configure Engram

Create the protected runtime config:

```sh
mkdir -p "$HOME/.engram"
install -m 0600 .env.example "$HOME/.engram/.env"
${EDITOR:-vi} "$HOME/.engram/.env"
```

Set these three required values:

```dotenv
TELEGRAM_BOT_TOKEN=the-token-from-BotFather
TELEGRAM_ALLOWED_USER_ID=the-message.from.id-integer
ANTHROPIC_API_KEY=your-Anthropic-key
```

Leave `TELEGRAM_CHAT_ID` empty for DM-only use. Engram then uses the allowed
user ID as the private chat ID. Never commit or post the completed env file.

### 4. Validate without network calls

Both commands load and validate the config without calling Telegram or
Anthropic and without starting polling. `dry-start` also creates and opens the
local state surface.

```sh
go run ./cmd/engram preflight --env "$HOME/.engram/.env"
go run ./cmd/engram dry-start --env "$HOME/.engram/.env"
```

Confirm that each command ends with `status: ok`, that `tmux` is not reported as
`missing`, and that the displayed user and chat IDs are your private DM IDs.

### 5. Start Engram

On Linux, install the binary and systemd user service:

```sh
make install-service PREFIX="$HOME/.local"
systemctl --user --no-pager --full status engram.service
```

On macOS, install and run it in a terminal instead:

```sh
make install PREFIX="$HOME/.local"
"$HOME/.local/bin/engram" run --env "$HOME/.engram/.env"
```

Only one Engram process may poll a configured bot/user/chat tuple. Do not run a
foreground copy while the systemd service is active.

### 6. Verify the first session

In the bot DM, send:

```text
/new pwd
```

Engram creates a tmux window, runs `pwd`, and replies with an editable session
anchor. The pane capture used for that anchor is sent to Anthropic; review the
privacy boundaries below before running commands that may print secrets.

## Configuration

`.env.example` is the complete configuration surface. The env file is a simple
`KEY=VALUE` file and must be a regular file with no group or other permissions.

| Setting | Default | Required | Purpose |
| --- | --- | --- | --- |
| `TELEGRAM_BOT_TOKEN` | none | yes, secret | Token issued by `@BotFather`. Treat it as access to the Engram control channel. |
| `TELEGRAM_ALLOWED_USER_ID` | none | yes | The one Telegram user ID allowed to issue commands. |
| `TELEGRAM_CHAT_ID` | allowed user ID | no | The one allowed chat. Leave empty for a private DM; group operation is unsupported. |
| `TELEGRAM_POLL_TIMEOUT_SECONDS` | `50` | no | Positive Telegram long-poll timeout in seconds. |
| `LLM_PROVIDER` | `anthropic` | no | Must remain `anthropic`. |
| `ANTHROPIC_API_KEY` | none | yes, secret | Credential used for Haiku status requests. |
| `ANTHROPIC_MODEL` | `claude-haiku-4-5-20251001` | no | Haiku model ID; the `claude-haiku-4-5` alias is also accepted. |
| `ENGRAM_HOME` | `~/.engram` | no | State, audit log, and process-lock directory. |
| `ENGRAM_WORKDIR` | `~` | no | Starting directory for new tmux sessions and windows. |
| `ENGRAM_TMUX_SESSION` | first existing session, otherwise `engram-<chat-id>` | no | Forces one tmux session name and creates it when absent. |
| `ENGRAM_ATTACHMENT_SOFT_MAX_BYTES` | `52428800` | no | Incoming attachment soft limit; larger files require an exact SHA-256 bypass. |

`make run` uses `~/.engram/.env` by default. For a protected local config at a
different path, override it explicitly:

```sh
chmod 600 "$PWD/.env"
make run ENGRAM_ENV="$PWD/.env"
```

The repository ignores only the root `.env`; prefer `~/.engram/.env`, and never
place alternate secret files in the checkout.

## Data Flow / Privacy

Engram deliberately connects a private chat, a local shell, and an external
model API. Compromise of the authorized Telegram account can become shell
access for the configured local user. A stolen bot token can expose or disrupt
the bot channel and must be revoked immediately.

- **Telegram:** Engram long-polls the Bot API for messages and attachments, then
  sends messages, anchor edits, and requested files back to the configured DM.
  Telegram receives command text, summaries, `/raw`, `/dump`, `/logs`, and
  `/download` results sent through the bot.
- **tmux and local processes:** Authorized messages can create windows and send
  literal shell input or key presses. tmux owns terminal history and continues
  running when Engram stops unless a window is explicitly closed.
- **Anthropic Haiku:** For an anchor refresh, Engram sends session metadata, a
  shortened last-input preview, the previous summary, and a bounded visible
  pane capture. Repeated lines may be omitted. If the first result is uncertain,
  Haiku may receive one bounded full-scrollback capture. Captures are not
  redacted before they are sent.
- **Local state and logs:** `ENGRAM_HOME` contains `state.json`, `audit.jsonl`,
  and lock files. State includes Telegram identifiers, session metadata, last
  input previews, the last raw visible capture, and Haiku summaries. Files are
  created with private permissions, but anyone with access to the host account
  can read them.
- **Attachments and generated files:** Incoming Telegram documents are saved
  under `/tmp/engram/attachments`. `/raw`, `/dump`, `/logs`, and command metadata
  create files under `/tmp/engram`. These files are not automatically removed
  by uninstall and may remain until manual or operating-system cleanup.
- **Downloads:** `/download <absolute-path>` uploads a local regular file to
  Telegram. It rejects symlinks, but it is still an intentional file-exfiltration
  command. Review the exact path before sending it.

Audit events redact configured credentials and common token, key, password, and
private-key patterns. `/logs` applies the same pattern-based redaction to a
bounded audit tail. Redaction can miss unfamiliar secrets or sensitive prose.
It does not sanitize terminal captures, `state.json`, `/raw`, `/dump`,
`/download`, incoming attachments, Telegram history, or Anthropic requests.
Treat all terminal transcripts and diagnostic artifacts as sensitive and review
them before sharing.

## Linux Lifecycle

Install or replace the binary:

```sh
make install PREFIX="$HOME/.local"
```

Install and start the systemd user service. This seeds `~/.engram/.env` with
mode `0600` only when it does not already exist:

```sh
make install-service PREFIX="$HOME/.local"
```

Operate and inspect the service:

```sh
systemctl --user status engram.service
systemctl --user stop engram.service
systemctl --user start engram.service
systemctl --user restart engram.service
journalctl --user -u engram.service
```

To keep the user service running after logout, enable lingering if that matches
the host's security policy:

```sh
loginctl enable-linger "$USER"
```

Update from a source checkout:

```sh
git pull --ff-only
make check
make install PREFIX="$HOME/.local"
systemctl --user restart engram.service
```

Remove the service before removing the binary:

```sh
make uninstall-service
make uninstall PREFIX="$HOME/.local"
```

Uninstall does not delete tmux sessions, `~/.engram`, or `/tmp/engram`. Review
and remove those separately only when their state, logs, and attachments are no
longer needed.

## macOS Lifecycle

Build, install, preflight, and foreground execution are supported:

```sh
make install PREFIX="$HOME/.local"
"$HOME/.local/bin/engram" preflight --env "$HOME/.engram/.env"
"$HOME/.local/bin/engram" run --env "$HOME/.engram/.env"
```

Stop the foreground process with `Ctrl+C`; tmux sessions remain. Engram does not
ship launchd integration, and `make install-service` and
`make uninstall-service` require Linux `systemctl`. A user-authored LaunchAgent
is outside the supported service lifecycle. Update by stopping Engram, updating
the checkout, running `make check` and `make install`, then starting it again.
Remove only the binary with:

```sh
make uninstall PREFIX="$HOME/.local"
```

## Commands

Use `/help` in Telegram for the complete command list, `/commands` for the
machine-readable metadata, or `engram commands` locally. Common commands are:

- `/sessions`
- `/attach <tmux-target>`
- `/new <text>`
- `/send <id> <text>`
- `/key <id> <keys...>`
- `/raw <id>`
- `/dump <id>`
- `/download <absolute-path>`
- `/logs`
- `/status`

Reply to a session anchor to send text to its pane. To send input beginning
with a slash, add one extra leading slash: replying with `//clear` sends
`/clear` and presses Enter.

Local diagnostics use the same protected env file:

```sh
engram preflight --env "$HOME/.engram/.env"
engram status --env "$HOME/.engram/.env"
engram dry-start --env "$HOME/.engram/.env"
```

## Development

Engram uses only the Go standard library. Run the full local gate before
pushing:

```sh
make check
```

The gate runs tests, `go vet`, Darwin compile checks, architecture and public
release checks, workflow checks, documentation checks, a tracked-file secret
scan, and a smoke build. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for change
guidance and [`SECURITY.md`](SECURITY.md) for private vulnerability reporting.

## License

Engram is open source under the MIT License. See [`LICENSE`](LICENSE).
