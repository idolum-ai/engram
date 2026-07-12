# Headless Operation

Engram has two deliberately different headless shapes:

1. the unattended Telegram service available today; and
2. a proposed local, read-only inspection command that makes no network calls.

Neither shape adds a daemon API, generic transport, or background command
inbox. Telegram remains Engram's product surface. Local inspection is a bounded
diagnostic for the machine already running tmux.

## Unattended Telegram Service

Status: available on Linux.

The normal headless deployment is one Engram process running as a systemd user
service. It long-polls Telegram, observes local tmux, and preserves state under
`~/.engram`. tmux continues running if Engram stops.

From a source checkout:

```sh
install -d -m 0700 "$HOME/.engram"
install -m 0600 .env.example "$HOME/.engram/.env"
${EDITOR:-vi} "$HOME/.engram/.env"
make install PREFIX="$HOME/.local"
```

The required Telegram values and presentation choices are documented in the
README configuration table. Check the local configuration before starting or
restarting the service:

```sh
"$HOME/.local/bin/engram" preflight --env "$HOME/.engram/.env"
"$HOME/.local/bin/engram" dry-start --env "$HOME/.engram/.env"
```

When both diagnostics end with `status: ok`, install and start the user service:

```sh
make install-service PREFIX="$HOME/.local"
```

Operate it without an attached terminal:

```sh
systemctl --user status engram.service
systemctl --user restart engram.service
journalctl --user -u engram.service
```

To keep the user service alive after logout, explicitly enable lingering when
that matches the host's security policy:

```sh
loginctl enable-linger "$USER"
```

Use `/status`, `/sessions`, and `/version` in the authorized Telegram DM to
verify the live process rather than only the binary on disk.

### Foreground Equivalent

Linux and macOS can run the same Telegram process without service integration:

```sh
engram preflight --env "$HOME/.engram/.env"
engram run --env "$HOME/.engram/.env"
```

This remains a Telegram-backed process. `Ctrl+C` stops Engram without closing
tmux sessions. Engram does not install a macOS LaunchAgent.

## Local Read-Only Inspection

Status: proposed; these commands are not implemented by this design PR.

After the private terminal-mechanics boundary is proven, Engram may expose:

```text
engram inspect status
engram inspect sessions
engram inspect frame <watch-id>
```

This is "headless" in the smaller sense: a one-shot local command emits bounded
plain text and exits. It does not poll Telegram, call Anthropic, launch
Chromium, open a listener, start a worker, or mutate tmux or Engram state.

### Intended Use

Check whether tmux and persisted Engram state can be read:

```sh
engram inspect status
```

List the watches already known to Engram, including their local IDs, immutable
tmux identity, provenance, and observed lifecycle state:

```sh
engram inspect sessions
```

Print one sanitized bounded literal frame by Engram watch ID:

```sh
engram inspect frame 3
```

The exact output format must be specified and tested in the implementation PR.
Human-readable output is not a stable machine protocol unless that PR says so.

### State Selection And Locking

Inspection uses `ENGRAM_HOME`, defaulting to `~/.engram`. It never accepts a bot
token, chat ID, Telegram message ID, arbitrary tmux target, or state path from
pane content.

The implementation must choose one conservative read strategy:

- read a proven atomic state snapshot while the Telegram service continues; or
- fail clearly when the exclusive state lock is held.

It must not stop, signal, or compete with the running service. A corrupt or
future-version state file fails closed and remains untouched.

### Deliberate Limits

Local inspection cannot:

- create, attach, rename, close, refresh, or send input;
- select an arbitrary tmux pane;
- print full scrollback, logs, attachments, or files;
- render Haiku prose or Chromium images;
- expose JSON-RPC, MCP, HTTP, sockets, or a filesystem command queue;
- act as a second current anchor or reply route.

For local interactive terminal work, use tmux itself. For remote phone control,
use the Telegram service. The inspection command exists only if it makes
Engram's tmux mechanics easier to diagnose and prove.

## Security Boundary

Both headless shapes run with the permissions of the local OS user and can see
that user's tmux panes. The Telegram service intentionally sends selected pane
content and files across configured external boundaries. The local inspection
command must make no network request and must sanitize terminal controls before
writing to stdout.

Neither mode protects against compromise of the owning OS account. Do not run
Engram under an account whose tmux sessions it should not observe.
