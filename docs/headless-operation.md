# Headless Operation

Engram has two deliberately different headless shapes:

1. the unattended Telegram service available today; and
2. a local, read-only inspection command that makes no network calls.

Neither shape adds a daemon API, generic transport, or background command
inbox. Telegram remains Engram's product surface. Local inspection is a bounded
diagnostic for the machine already running tmux.

## Unattended Telegram Service

Status: available on Linux and macOS.

The normal headless deployment is one Engram process running as a systemd user
service on Linux or LaunchAgent on macOS. It long-polls Telegram, observes local
tmux, and preserves state under `~/.engram`. `KillMode=process` on Linux and
`AbandonProcessGroup` on macOS preserve tmux descendants across Engram stops.

### Install A Published Binary

Choose a version from GitHub Releases, inspect the installer at that same tag,
then run it. The installer verifies the archive checksum and embedded version
before atomically replacing the binary:

```sh
version=vX.Y.Z # replace with the release you reviewed
curl -fsSLo /tmp/engram-install-release.sh \
  "https://raw.githubusercontent.com/idolum-ai/engram/${version}/scripts/install-release.sh"
less /tmp/engram-install-release.sh
bash /tmp/engram-install-release.sh "${version}"
```

Release installation does not modify `~/.engram`, create a service, or restart
one. Use a source checkout for the initial configuration and native service
definition:

```sh
make install-service-unit PREFIX="$HOME/.local"
```

Installing or replacing a binary never restarts the running process. Stop the
service first when automatic failure recovery must not activate the replacement
before the planned restart.

### Install From Source

From a source checkout:

```sh
install -d -m 0700 "$HOME/.engram"
install -m 0600 .env.example "$HOME/.engram/.env"
${EDITOR:-vi} "$HOME/.engram/.env"
make install PREFIX="$HOME/.local"
```

The required Telegram values and presentation choices are documented in
[Configuration](configuration.md). Check the local configuration before
starting or restarting the service:

```sh
"$HOME/.local/bin/engram" preflight --env "$HOME/.engram/.env"
"$HOME/.local/bin/engram" dry-start --env "$HOME/.engram/.env"
```

When both diagnostics end with `status: ok`, install the user service. Linux
starts its systemd unit as before; macOS uses an explicit start:

```sh
make install-service PREFIX="$HOME/.local"
# macOS only:
make service-start PREFIX="$HOME/.local"
```

Operate it without an attached terminal:

```sh
make service-start PREFIX="$HOME/.local"
make service-stop PREFIX="$HOME/.local"
make service-status PREFIX="$HOME/.local"
make service-restart PREFIX="$HOME/.local"
make service-logs PREFIX="$HOME/.local"
ENGRAM_LOG_LINES=1000 make service-logs PREFIX="$HOME/.local"
```

`ENGRAM_LOG_LINES` accepts a bounded value from 1 through 1000 and defaults to
200.

`service-status` matches the service manager's live PID to an owner-only
identity record written by that running Engram process. The reported build is
therefore the active process build, not merely the version of the currently
installed binary or service definition.

Service installation also resolves the exact `tmux` executable from the
installer's PATH and records an explicit service PATH containing that directory.
This is required on macOS, where launchd does not inherit Homebrew's PATH.

`service-restart` is also the service-definition migration boundary: it first
regenerates and validates the LaunchAgent or systemd unit from the installed
binary and current environment path, then performs the explicit restart. Merely
replacing the binary still does not activate or restart the service.

### Upgrade And Roll Back

From a source checkout:

```sh
git pull --ff-only
make check
make install PREFIX="$HOME/.local"
make service-restart PREFIX="$HOME/.local"
make service-status PREFIX="$HOME/.local"
```

For rollback, install the previously reviewed binary, restart explicitly, and
verify `make service-status` plus Telegram `/status`.

Remove the service before removing the binary:

```sh
make uninstall-service PREFIX="$HOME/.local"
make uninstall PREFIX="$HOME/.local"
```

Uninstall does not remove tmux sessions, `~/.engram`, or the private runtime
root. Review those separately when their state and attachments are no longer
needed.

To keep the user service alive after logout, explicitly enable lingering when
that matches the host's security policy:

```sh
loginctl enable-linger "$USER"
```

Use `/status` in the authorized Telegram DM to verify the live application and
`/sessions` to inspect its tracked work. The local `engram version` command
identifies a binary on disk; it does not prove that binary is the running
service.

### Foreground Equivalent

Linux and macOS can run the same Telegram process without service integration:

```sh
engram preflight --env "$HOME/.engram/.env"
engram run --env "$HOME/.engram/.env"
```

This remains a Telegram-backed process. `Ctrl+C` stops Engram without closing
tmux sessions. Do not run it while the native user service is active.

## Agent Compatibility Doctor

`engram doctor agent [--provider codex|claude] [--pane %N]` is a second local,
read-only diagnostic. It uses the production process, screen, binding, and
transcript probes while omitting terminal text, paths, UUIDs, task text, and
agent names. It writes no Engram state or pane metadata and makes no network
calls. See [Agent compatibility](agent-compatibility.md).

## Local Read-Only Inspection

Status: available on Linux and macOS.

Engram exposes:

```text
engram inspect status
engram inspect sessions
engram inspect frame <watch-id>
```

This is "headless" in the smaller sense: a one-shot local command emits bounded
plain text and exits. It does not poll Telegram, call a model provider, launch
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

Print one control-safe bounded literal frame by Engram watch ID:

```sh
engram inspect frame 3
```

Output is bounded human-readable text, not a stable machine protocol.

### State Selection And Locking

Inspection uses `ENGRAM_HOME`, defaulting to `~/.engram`. It never accepts a bot
token, chat ID, Telegram message ID, arbitrary tmux target, or state path from
pane content.

Inspection reads the complete state file produced by Engram's atomic replacement
path while the Telegram service continues. It takes no writer lock because it
cannot write. It does not create a missing state file, migrate an old one,
replace a corrupt one, change permissions, or leave recovery files. Symlinks,
non-regular files, oversized files, malformed JSON, and future schema versions
fail closed and remain untouched.

It does not stop, signal, or compete with the running service.

### Deliberate Limits

Local inspection cannot:

- create, attach, rename, close, refresh, or send input;
- select an arbitrary tmux pane;
- print full scrollback, logs, attachments, or files;
- render model prose or Chromium images;
- expose JSON-RPC, MCP, HTTP, sockets, or a filesystem command queue;
- act as a second current anchor or reply route.

For local interactive terminal work, use tmux itself. For remote phone control,
use the Telegram service. The inspection command exists only if it makes
Engram's tmux mechanics easier to diagnose and prove.

## Security Boundary

Both headless shapes run with the permissions of the local OS user and can see
that user's tmux panes. The Telegram service intentionally sends selected pane
content and files across configured external boundaries. The inspector process
constructs no network client and makes no direct network request. It removes
terminal and Unicode presentation controls before writing to stdout, but it
does not redact pane text or secrets. Invoking tmux can also run hooks configured
by the owning user; Engram does not claim those tmux-side effects as part of its
no-client boundary.

Neither mode protects against compromise of the owning OS account. Do not run
Engram under an account whose tmux sessions it should not observe.
