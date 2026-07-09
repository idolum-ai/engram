# Engram Spec

Status: draft

## Summary

Engram is a Telegram bot/service that lets a Telegram chat act as a lightweight
client for tmux-backed terminal work, with Haiku-summarized terminal anchors.

A normal top-level Telegram message starts a new managed terminal session. The
service creates a new tmux window, sends the message as terminal input, and
replies with a Telegram message labeled with a stable handle such as `[1]`.
That reply becomes the navigational anchor for the terminal.

When the user replies to the bot's `[1]` message with `ls`, the service sends
`ls` to the tmux pane backing `[1]`. When the pane output changes, the service
edits the existing `[1]` Telegram message with an LLM-simplified summary of the
latest terminal state. If nothing changed, the service does not send or edit
anything.

The service also exposes a `/sessions` command that shows active managed
terminal sessions with Telegram inline buttons. Clicking a button begins or
resumes watching that session in the current chat.

## Terminology

This project should keep tmux terminology precise internally while using simple
Telegram-facing names.

- **tmux session**: native tmux session object.
- **tmux window**: native tmux window object inside a tmux session.
- **tmux pane**: native tmux pane object where shell input/output happens.
- **terminal session**: Telegram-facing object, shown as `[1]`, `[2]`, `[3]`.
  Internally this is one managed tmux window with one primary pane.
- **anchor message**: the bot's Telegram message for a terminal session. The
  service edits this message as terminal output changes.
- **watch**: a user's request for the service to keep updating an anchor message
  for a terminal session.

Initial internal model:

- One managed tmux session per Telegram chat.
- One tmux window per Telegram terminal session.
- One primary pane per managed window.

This preserves the user's requested "new message creates a new tmux window"
behavior while keeping `/sessions` useful as the Telegram product term.

## User Experience

### Start A New Session

User sends a top-level non-command message:

```text
hi
```

Service behavior:

1. Allocates the next terminal session handle, for example `[1]`.
2. Ensures the chat's managed tmux session exists.
3. Creates a new tmux window for `[1]`.
4. Sends the message text to the pane, followed by Enter.
5. Sends a Telegram reply anchor for `[1]` with the current Haiku summary.
6. Records the anchor message ID for future edits.

Example bot response:

```text
[1] running
tmux: tg_12345:@7.%12

Haiku summary:
- The shell tried to run `hi`.
- It failed because `hi` is not an installed command.

Latest prompt:
$ 
```

### Send Input To An Existing Session

User replies to the bot's `[1]` anchor message:

```text
ls
```

Service behavior:

1. Resolves the replied-to Telegram message ID to terminal session `[1]`.
2. Sends `ls` to the backing tmux pane, followed by Enter.
3. Marks `[1]` dirty.
4. Edits the existing `[1]` anchor message when the rendered output changes.

Plain replies are optimized for shell command entry. For terminal UI programs,
the user should use explicit key commands when they need navigation or control
keys.

### Terminal UI Programs

If a command opens a terminal UI, such as `vim`, `less`, `top`, `htop`, `tig`,
`fzf`, an installer, or a curses menu, the service keeps the same terminal
session open and summarizes the current visible screen.

The anchor should make it clear that the pane appears to be in an interactive
terminal UI instead of a shell prompt:

```text
[1] running  tui
last input: vim README.md
updated: 02:49:12 UTC

summary:
- Vim is open on README.md.
- The cursor appears near the top of the file.
- The editor is waiting for keyboard input.

keys:
- /key 1 Esc
- /key 1 :wq Enter
- /text 1 inserted text
```

Input rules:

- Replying with plain text still sends that text followed by Enter.
- `/key <id> <keys...>` sends tmux key names through `send-keys`, for example
  `/key 1 q`, `/key 1 C-c`, `/key 1 Escape`, `/key 1 Down Enter`.
- `/text <id> <text>` sends literal text without appending Enter.
- `/send <id> <text>` remains a command-oriented alias that sends literal text
  followed by Enter.
- Attachments are still only saved to `/tmp`; they are not pasted into a TUI
  automatically.

The service should not attempt to fully emulate the TUI. Tmux remains the source
of truth; the bot only captures the visible pane, asks Haiku for a faithful
phone-sized rendering, and sends explicit user keystrokes back to tmux.

### List Sessions

User sends:

```text
/sessions
```

Service responds with active terminal sessions and buttons:

```text
Active sessions

[1] running  ~/repo  last: ls
[2] idle     ~/repo  last: npm test
[3] exited   /tmp    last: python script.py
```

Buttons:

- `Watch [1]`
- `Watch [2]`
- `Open [3]`
- `Close [n]`

Clicking `Watch [1]` causes the service to edit the existing `[1]` anchor
message if it exists in the chat. If there is no usable anchor message, the
service sends a new anchor message and records it.

### Dump Full Scrollback

User sends:

```text
/dump 1
```

Service behavior:

1. Resolves `1` to terminal session `[1]`.
2. Captures the full available scrollback for the backing tmux pane.
3. Returns the dump as a Telegram document when it is too large for a message.
4. Returns a normal text message only when the dump is small enough.

The dump is a point-in-time artifact, not a watched anchor. It should not replace
or mutate the existing `[1]` anchor message.

Suggested artifact name:

```text
tmux-session-1-2026-07-09T024103Z.txt
```

### Close A Session

User sends:

```text
/close 1
```

Service behavior:

1. Resolves `1` to terminal session `[1]`.
2. Kills the backing tmux window.
3. Marks `[1]` as `closed`.
4. Disables watching for `[1]`.
5. Edits the existing anchor message one final time with a local closed status.

Example final anchor:

```text
[1] closed
last input: ls
closed: 02:45:10 UTC

summary:
- Session closed by request.
```

`/close` should be the normal user-facing command. `/kill` can remain reserved
for a later force-close flow if the tmux target is wedged or out of sync.

### Receive Attachments

When the allowed Telegram user sends an attachment in the configured group chat,
the service downloads it through the Telegram Bot API and stores it under `/tmp`.

Suggested storage path:

```text
/tmp/engram/attachments/<timestamp>-<telegram-file-id>-<safe-name>
```

Service behavior:

1. Verifies the update came from the configured user and group chat.
2. Downloads the Telegram file.
3. Writes the file under the attachment directory in `/tmp`.
4. Records the attachment metadata in local state.
5. Replies in Telegram with the absolute path.

Example response:

```text
attachment saved
/tmp/engram/attachments/20260709T024510Z-AAE42-report.txt
```

The service should not feed attachment contents into a tmux pane automatically.
The path is sent back to Telegram so the user can copy it into a session, use
`/send <id> <path>`, or inspect it with `/attachments`.

### List Attachments

User sends:

```text
/attachments
```

Service behavior:

1. Lists the attachment directory under `/tmp`.
2. Replies with filenames, sizes, modified times, and absolute paths.
3. Shows an empty state if no attachments have been received.

This is intentionally close to `ls` for the attachment folder, not a rich file
manager.

### Download Local File To Telegram

User sends:

```text
/download /tmp/engram/attachments/20260709T024510Z-AAE42-report.txt
```

Service behavior:

1. Requires an absolute path.
2. Does not expand shell syntax, environment variables, globs, or `~`.
3. Verifies the path points to a regular readable file.
4. Uploads that exact file to the configured Telegram group chat.
5. Replies with success or a precise local error.

`/download <path>` is intentionally powerful: it can upload any readable local
file by absolute path, not just files from the attachment directory. It must only
be accepted from the single configured user.

### No Duplicate Live Messages

The service must not send a new Telegram message for every output event. Each
terminal session should prefer one anchor message per chat and update that
message in place.

The service skips Telegram edits when:

- The rendered summary hash is unchanged.
- The session is not watched.
- The pane has no new output and no status transition.
- The update would exceed configured rate limits.
- Fewer than 10 seconds have passed since the last anchor edit for that session,
  unless this is a final state change such as `closed`, `exited`, or `lost`.

## Telegram Commands

Initial command set:

- `/sessions`: list active terminal sessions with buttons.
- `/new <text>`: explicitly create a new terminal session and send `<text>`.
- `/send <id> <text>`: send input to a terminal session without replying.
- `/text <id> <text>`: send literal text without appending Enter.
- `/key <id> <keys...>`: send tmux key names for terminal UI control.
- `/watch <id>`: create or resume an anchor message for a terminal session.
- `/dump <id>`: return the full available scrollback for a terminal session.
- `/close <id>`: close the backing tmux window and stop watching the session.
- `/attachments`: list files saved from Telegram attachments under `/tmp`.
- `/download <absolute-path>`: upload the exact local file to the configured
  Telegram group chat.
- `/stop <id>`: stop watching without killing the tmux target.
- `/quit`: stop Engram cleanly without closing tmux sessions.
- `/restart`: request a clean service restart.
- `/kill <id>`: reserved for future force-close behavior after confirmation.
- `/help`: show concise usage.

Plain top-level text remains the fastest path for creating a new terminal
session.

Reply input should be the primary ergonomic path for continuing a session.

## Implementation Constraints

This should be a small Go system.

- Language: Go.
- Shape: one installable command-line binary.
- Install path: clone the repo, then run `make install`.
- Service install: `make install-service` installs and enables a systemd user
  service so Engram recovers after machine restart and user login.
- Runtime config: local `.env` file, ignored by git.
- LLM provider: Anthropic only.
- LLM model family: Haiku only.
- Default model ID: `claude-haiku-4-5-20251001`.
- Provider abstraction: do not build a generic multi-provider layer for MVP.
- Dependencies: Go standard library only. Do not add third-party Go modules for
  Telegram, Anthropic, dotenv, locking, logging, JSON, or systemd helpers.

The implementation should use the Go standard library where practical:

- Telegram Bot API calls can use `net/http`.
- Anthropic Messages API calls can use `net/http` in non-streaming request/
  response mode.
- Configuration can be loaded from `.env` with a tiny local parser.
- State can be persisted with atomic JSON files for MVP.
- Locking should use standard library file and syscall primitives available on
  Linux.

Avoid a database and all third-party dependencies for MVP.

### Makefile Contract

The repo should be installable with:

```sh
make install
```

Suggested targets:

- `make build`: build the binary into `bin/`.
- `make install`: build and copy the binary to `$(PREFIX)/bin`.
- `make uninstall`: remove the installed binary.
- `make install-service`: install and enable the systemd user service.
- `make uninstall-service`: disable and remove the systemd user service.
- `make test`: run Go tests.
- `make run`: run locally with `.env`.

Default `PREFIX` should be `/usr/local`, overridable by the user:

```sh
make install PREFIX="$HOME/.local"
```

### Configuration

`.env` is local operator state and must not be committed.

Required variables:

```sh
TELEGRAM_BOT_TOKEN=
TELEGRAM_ALLOWED_USER_ID=
TELEGRAM_GROUP_CHAT_ID=
TELEGRAM_API_ID=
TELEGRAM_API_HASH=
LLM_PROVIDER=anthropic
ANTHROPIC_API_KEY=
ANTHROPIC_MODEL=claude-haiku-4-5-20251001
ENGRAM_HOME=~/.engram
ENGRAM_WORKDIR=~
ENGRAM_ATTACHMENT_SOFT_MAX_BYTES=52428800
```

`TELEGRAM_BOT_TOKEN` is the MVP credential for Bot API operation. Telegram app
credentials are usually called API ID and API hash rather than client ID and
client secret; the `.env` should still allow them through `TELEGRAM_API_ID` and
`TELEGRAM_API_HASH` so later MTProto/user-client features do not require a
configuration rename.

Startup validation:

- `TELEGRAM_BOT_TOKEN` is required for MVP Bot API mode.
- `TELEGRAM_ALLOWED_USER_ID` must contain exactly one Telegram user ID.
- `TELEGRAM_GROUP_CHAT_ID` must contain exactly one Telegram group or supergroup
  chat ID.
- The service must reject every update not sent by `TELEGRAM_ALLOWED_USER_ID`.
- The service must reject group messages outside `TELEGRAM_GROUP_CHAT_ID`.
- `TELEGRAM_API_ID` and `TELEGRAM_API_HASH` are optional in MVP Bot API mode.
- `LLM_PROVIDER` must be exactly `anthropic`.
- `ANTHROPIC_MODEL` must be exactly the configured Haiku model ID or an allowed
  Anthropic Haiku alias. Initially allowed values are
  `claude-haiku-4-5-20251001` and `claude-haiku-4-5`.
- The service must reject Sonnet, Opus, Fable, Mythos, or any non-Anthropic
  provider.
- Missing `ANTHROPIC_API_KEY` disables startup, not just summarization.
- `ENGRAM_HOME` defaults to `~/.engram`.
- `ENGRAM_WORKDIR` defaults to `~` and is used as the cwd for new tmux windows.
- `ENGRAM_ATTACHMENT_SOFT_MAX_BYTES` is a soft inbound attachment limit.

### Service Contract

Engram should install as a systemd user service for crash and reboot recovery.

Suggested unit name:

```text
engram.service
```

Suggested unit behavior:

- `ExecStart=%h/.local/bin/engram run --env %h/.engram/.env`
- `Restart=on-failure`
- `RestartSec=5`
- starts after the user manager is available
- uses the operator's normal login environment as little as practical

`make install-service` should:

1. Install the binary.
2. Create `~/.config/systemd/user/engram.service`.
3. Run `systemctl --user daemon-reload`.
4. Enable and start the service.

The operator may also run:

```sh
loginctl enable-linger "$USER"
```

if they want the user service to start at boot before an interactive login.
Engram should document this rather than run it automatically.

## Architecture

### Components

- **Telegram ingress**: long polling receiver for messages and callback queries.
- **Router**: classifies updates as command, new session request, reply input,
  attachment, or button callback.
- **Session registry**: durable mapping between Telegram chats/messages and tmux
  targets.
- **Tmux controller**: owns tmux commands, control-mode client, pane output
  events, and target lifecycle.
- **Attachment store**: downloads Telegram attachments into `/tmp`, lists them,
  and records metadata.
- **Download sender**: uploads an exact local absolute path to the configured
  Telegram group chat.
- **LLM simplifier**: sends compact terminal snapshots and deltas to Anthropic
  Haiku and receives Telegram-sized summaries.
- **Renderer**: converts Haiku summaries plus small local metadata into
  Telegram-safe text.
- **Update scheduler**: coalesces pane output events and edits anchor messages
  at a configured refresh rate.
- **Authorization policy**: restricts which Telegram users/chats may create,
  read, and kill terminal sessions.
- **Instance lock**: prevents two Engram processes with the same Telegram bot
  token/user/group settings from polling at the same time.

### Tmux Integration

Use a long-lived tmux control-mode client as the change detector and tmux
commands for lifecycle operations.

New tmux windows should start in `ENGRAM_WORKDIR`, which defaults to `~`.

Useful tmux operations:

- Create managed chat session:
  `new-session -d -s <chat-session-name>`
- Create terminal session window:
  `new-window -P -F "#{session_id} #{window_id} #{pane_id}" -c <workdir> -t <chat-session>`
- Send literal input:
  `send-keys -t <pane-id> -l -- <text>`
- Send Enter:
  `send-keys -t <pane-id> Enter`
- Send TUI/navigation keys:
  `send-keys -t <pane-id> <key> ...`
- Capture render snapshot:
  `capture-pane -p -e -J -t <pane-id>`
- Capture full available scrollback for `/dump`:
  `capture-pane -p -e -J -S - -E - -t <pane-id>`
- List managed targets:
  `list-windows` and `list-panes` with explicit formats
- Close a terminal session:
  `kill-window -t <window-id>`

Control mode provides `%output` notifications when a pane produces output. The
service should treat these notifications as a dirty signal, then capture the
current pane state with `capture-pane` for LLM simplification. This avoids
trying to reconstruct a full terminal emulator from raw output while still
avoiding blind polling.

### LLM Simplification

Anchor messages should not be raw terminal buffers. They should be short Haiku
summaries of what changed and what the terminal appears to need next.

The simplifier input should include:

- Terminal session handle, for example `[1]`.
- Current state: `running`, `idle`, `exited`, `lost`, `closed`, or `killed`.
- Last user input.
- Input mode hint: shell-like command, literal text, or explicit key input.
- Previous summary, if available.
- Current visible capture.
- Optional small tail of recent raw output since the last summary.

The simplifier output should be constrained to a compact schema:

```text
status: running
summary:
- ...
next:
- ...
prompt: "$"
```

The system prompt should frame the model as a readable screen renderer, not as a
general assistant:

```text
Imagine you are a small phone screen re-rendering a terminal. Preserve the
meaning, current state, errors, prompts, and actionable output faithfully, but
compress the raw buffer into a readable Telegram message. Do not invent success,
files, commands, or next steps that are not supported by the visible buffer.
```

### LLM Token Budget

`max_tokens` is an output cap, not the total request size. The request budget
should be planned as:

```text
system prompt budget
+ visible tmux capture budget
+ optional recent-output delta budget
+ previous-summary budget
+ max reply budget
```

The implementation should size the prompt inputs around the current visible tmux
buffer, then set `max_tokens` to the maximum useful Telegram reply size. A good
default target is "what can be read comfortably on an iPhone screen without
scrolling much", not the largest message Telegram permits.

Initial defaults:

- Capture size sent to Haiku: 100 columns by 40 rows.
- Visible capture budget: roughly the captured character count, converted to a
  conservative token estimate before sending.
- Previous summary budget: 800 characters.
- Recent-output delta budget: 2,000 characters.
- `max_tokens`: 700.
- Hard rendered anchor target: 2,000 characters.
- Absolute rendered anchor ceiling: 3,500 characters.

If the visible buffer is too large, crop from the top and keep the bottom of the
terminal, because the prompt and latest command output usually matter most. If a
single command produces important earlier output, `/dump <id>` is the raw escape
hatch.

Rules:

- Use Anthropic Haiku only.
- Use the non-streaming Anthropic Messages API only. Omit `stream` or set it to
  `false`; do not use server-sent events or partial token streaming.
- Do not use the LLM for authorization decisions.
- Do not send the full scrollback to Haiku by default.
- Prefer deltas plus visible pane captures to control token use.
- If Haiku fails, keep the last successful summary and add a local stale marker.
- `/dump <id>` remains the raw full-buffer path and does not use the LLM.
- Never imply a command succeeded unless the terminal output supports it.
- Do not edit the Telegram anchor until the full Haiku response has been
  received, parsed, and rendered.
- If the visible buffer appears to be a TUI, summarize the screen state and
  useful key options; do not pretend the application has returned to a shell.

### Update Loop

For each watched terminal session:

1. Tmux control-mode output marks the pane dirty.
2. Scheduler waits for a short debounce window.
3. Controller captures the pane.
4. If the raw capture hash changed, the simplifier makes one non-streaming Haiku
   request for a complete new summary.
5. Renderer builds Telegram text from metadata and the Haiku summary.
6. If the rendered hash differs from the last sent hash, edit the anchor message.
7. Store the new hashes, edit timestamp, model ID, and status.

Recommended defaults:

- Debounce: 500 ms to 1 s.
- Minimum edit interval per anchor: 10 s.
- Maximum forced refresh interval while dirty: 10 s, and only when the rendered
  summary hash changed.
- No edit when unchanged.

Anchor edits should be intentionally slow. Fast terminal output should collapse
into at most one meaningful Telegram edit per watched terminal session every 10
seconds. The exact values should be configurable because Telegram rate limits and
terminal output patterns vary, but 10 seconds is the MVP default.

## Data Model

Suggested durable storage for MVP: atomic JSON files under the user's state
directory. This keeps the Go binary small and avoids database dependencies.

Suggested paths:

- Home: `~/.engram`
- Config: `~/.engram/.env`
- State: `~/.engram/state.json`
- Audit log: `~/.engram/audit.jsonl`
- Locks: `~/.engram/locks/`

Database-backed state can be reconsidered later if concurrent writers or query
complexity make the JSON registry painful.

### Instance Locking

Engram must prevent duplicate pollers for the same Telegram settings. On startup
it should derive a lock key from:

- `TELEGRAM_BOT_TOKEN`
- `TELEGRAM_ALLOWED_USER_ID`
- `TELEGRAM_GROUP_CHAT_ID`

The lock key must be hashed before being used in a filename. The process should
hold an exclusive lock file such as:

```text
~/.engram/locks/<sha256>.lock
```

If another live process already holds the lock, startup must fail with a clear
message. This protects Telegram polling, state writes, and anchor edits from two
copies of the same service.

### terminal_sessions

- `id`: local integer handle shown as `[id]`.
- `chat_id`: Telegram chat ID.
- `created_by_user_id`: Telegram user ID.
- `tmux_session_name`: managed tmux session name.
- `tmux_session_id`: tmux session ID, if known.
- `tmux_window_id`: tmux window ID.
- `tmux_pane_id`: tmux pane ID.
- `title`: display title, derived from first input, cwd, or user rename.
- `state`: `running`, `idle`, `exited`, `lost`, `closed`, `killed`.
- `created_at`, `updated_at`, `last_activity_at`.
- `last_input_preview`: short preview of the last user input.
- `last_input_mode`: `command`, `text`, or `keys`.
- `last_raw_capture_hash`: hash of the last captured visible pane.
- `last_summary_hash`: hash of the last Haiku summary.
- `last_render_hash`: hash of the last Telegram render.
- `last_summary`: last successful Haiku summary text or structured payload.
- `last_summary_model`: Anthropic Haiku model ID used for the summary.
- `anchor_chat_id`: Telegram chat containing the current anchor message.
- `anchor_message_id`: Telegram message ID to edit.
- `watch_enabled`: boolean.

### input_events

- `id`
- `terminal_session_id`
- `telegram_message_id`
- `user_id`
- `text`
- `sent_at`
- `status`: `sent`, `failed`
- `error`

### render_events

- `id`
- `terminal_session_id`
- `telegram_message_id`
- `render_hash`
- `rendered_at`
- `status`: `edited`, `sent_new_anchor`, `skipped_unchanged`, `failed`
- `error`

### attachments

- `id`
- `telegram_file_id`
- `telegram_unique_file_id`
- `chat_id`
- `user_id`
- `original_name`
- `content_type`
- `size_bytes`
- `sha256`
- `stored_path`
- `received_at`
- `bypass_requested`: boolean

### download_events

- `id`
- `requested_path`
- `chat_id`
- `user_id`
- `telegram_message_id`
- `sent_at`
- `status`: `sent`, `failed`
- `error`

## Rendering

Render anchor messages as compact LLM-summarized terminal cards:

```text
[1] running  ~/repo
last input: ls
updated: 02:41:03 UTC

summary:
- Listed the current directory.
- Found README.md, go.mod, and cmd/.

next:
- Reply here with the next shell command.
```

Rendering rules:

- Keep the handle `[n]` at the top.
- Include enough tmux target metadata for debugging, but keep it short.
- Prefer plain text or Telegram HTML with escaped content.
- Keep live anchors concise enough to fit comfortably in Telegram.
- Do not include raw terminal buffers in live anchors by default.
- Strip unsupported ANSI before sending terminal excerpts to Haiku.
- Include the last prompt or a short final line when it helps navigation.

The service should use `capture-pane` for the visible snapshot sent to Haiku and
avoid sending the full scrollback by default.

`/dump <id>` is the explicit escape hatch for full scrollback. It should capture
from the start of the tmux history through the current pane end and send the
result as a downloadable text file when needed. The renderer should include a
small header with session handle, tmux target, capture time, and truncation
status if the service enforces a maximum dump size.

## Lifecycle And Recovery

On service startup:

1. Load registry rows for non-closed terminal sessions.
2. Verify each tmux target still exists.
3. Mark missing targets as `lost`.
4. Reconnect the tmux control-mode client.
5. Re-enable output notifications for watched panes.
6. Refresh anchor messages only if their rendered state changed.

If a Telegram edit fails because the anchor message is unavailable, the service
should send a new anchor message and update `anchor_message_id`.

If a tmux pane exits:

- Mark the terminal session `exited`.
- Render the final Haiku summary once.
- Keep the session visible in `/sessions` until explicitly removed or pruned.

If a user closes a terminal session with `/close <id>`:

- Kill the backing tmux window if it still exists.
- Mark the terminal session `closed`.
- Stop all watch updates for that terminal session.
- Leave the closed session visible in `/sessions` until pruned so old anchors
  remain understandable.

If a user sends `/quit`:

- Stop accepting new Telegram updates.
- Flush state and audit logs.
- Exit the process with status 0.
- Do not close tmux sessions.
- The systemd service should not restart after a clean exit.

If a user sends `/restart`:

- Flush state and audit logs.
- Exit with a restart-specific nonzero status or exec a fresh process.
- The systemd service should bring Engram back up.
- Do not close tmux sessions.

### Attachment Limits

Inbound attachments have a soft size limit from
`ENGRAM_ATTACHMENT_SOFT_MAX_BYTES`.

If an attachment exceeds the soft limit:

1. Do not download or store it by default.
2. Reply with the configured soft limit.
3. Reply with available disk space for `/tmp/engram`.
4. Ask the user to resend the exact same attachment with an explicit bypass
   command or caption containing the expected hash.

Suggested bypass:

```text
/attachment-bypass sha256:<hash>
```

For a bypass, Engram should verify the downloaded file hash before accepting it.
If the hash does not match, delete the file and report the mismatch.

## Security

This service gives Telegram users shell access through tmux, so authorization is
not optional.

Minimum policy:

- Exactly one configured Telegram user ID.
- Exactly one configured Telegram group chat ID.
- Single configured group chat isolation.
- No shared global shell unless configured.
- Treat `/close` as an explicit destructive command; group chats may require an
  inline confirmation button before closing.
- Treat `/download <path>` as a sensitive local file exfiltration command.
  It must require the configured user and must send only to the configured group
  chat.
- Incoming attachments must be written under `/tmp/engram`.
  Filenames must be sanitized; paths in replies must be absolute.
- Large attachment bypass must require an exact SHA-256 match.
- Confirmation for future force-close commands such as `/kill`.
- Redaction policy for environment variables and secrets in diagnostic output.
- Audit log for session creation, input sent, attachment saves, downloads, close
  actions, and auth failures.

Optional later policies:

- Read-only watch users.
- Per-session owner checks.
- Command denylist or approval flow.
- Sandboxed shell profile for created windows.

## MVP Scope

MVP should support:

- Telegram long polling.
- Exactly one configured Telegram user and one configured group chat.
- One managed tmux session for the configured group chat.
- Systemd user service install and startup recovery.
- `~/.engram` config, state, audit, and lock files.
- Instance locking by Telegram bot/user/group settings.
- New tmux windows default to `~`, configurable by `ENGRAM_WORKDIR`.
- Plain text creates a new terminal session/window.
- Bot sends one Haiku-summarized anchor message per terminal session.
- Replies to anchor messages send input to the correct pane.
- `/key <id> <keys...>` and `/text <id> <text>` support terminal UI programs.
- `/sessions` lists active sessions with inline watch buttons.
- `/dump <id>` returns full available pane scrollback as text or a file.
- `/close <id>` closes the backing tmux window and marks the session closed.
- Incoming Telegram attachments are copied under `/tmp` and replied to with an
  absolute local path.
- `/attachments` lists the attachment folder.
- `/download <absolute-path>` uploads the exact local file to the configured
  Telegram group chat.
- `/help` shows concise command help.
- `/quit` stops Engram cleanly without closing tmux sessions.
- `/restart` restarts Engram through the service path without closing tmux
  sessions.
- Large attachments are blocked by a soft limit unless resent with exact hash
  bypass.
- Dirty-driven anchor edits using tmux control-mode output plus `capture-pane`.
- Durable JSON registry.
- `make install` installation path.
- Local `.env` config ignored by git.
- Anthropic Haiku-only simplification.
- Basic startup recovery.

MVP can defer:

- Multi-pane layouts.
- Rich terminal color rendering.
- Interactive full scrollback browsing beyond `/dump`.
- Multiple anchors per same terminal session.
- Collaborative permissions beyond the one-user, one-group model.
- Generic LLM provider support.
- Database-backed state.
- Third-party Go dependencies.

## Open Questions

- Should `/sessions` show only Engram terminal sessions, or also expose native
  tmux sessions through a separate `/tmux` command?
- Should button clicks edit the original anchor message, send a fresh anchor
  near the `/sessions` command, or both?
- How long should exited sessions remain in `/sessions`?
- Should large attachment bypass use caption syntax only, `/attachment-bypass`,
  or both?
- Should `/restart` exec in-process when not running under systemd?
