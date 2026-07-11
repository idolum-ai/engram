# Operations Requirements

Engram should be simple to install, inspect, update, and remove without hiding
runtime state.

## Configuration And Local Runs

- The canonical runtime env file is `~/.engram/.env` with mode `0600`.
- `make run` must use `ENGRAM_ENV`, defaulting to `$(HOME)/.engram/.env`.
- A local run may override `ENGRAM_ENV` with another protected regular file.
- `.env.example` and the README configuration table must describe the complete
  supported configuration surface.
- `ENGRAM_ANCHOR_MODE` selects the startup presentation and fallback when no
  valid persisted mode exists. State schema v6 persists runtime mode changes.
- The effective startup mode must be available: `guide` requires an Anthropic
  Haiku configuration; `snapshot` requires a successful bounded, ephemeral
  Chromium render. Engram does not call Anthropic merely to probe credentials.
- Optional dependencies are checked at startup. A configured Haiku client or
  probed renderer enables its corresponding alternate view and `/mode` target.
- `/mode [guide|snapshot]` persists the selection and begins anchor migration
  without restarting. Its response says switching has begun; each anchor keeps
  its current format until migration succeeds.
- `ENGRAM_SNAPSHOT_BROWSER` may name or point to a Chromium-compatible
  executable. When unset, Engram searches common Chromium and Chrome names and
  standard macOS application paths.

## Linux Installation And Service

- `make build` builds the binary and `make install` installs to
  `$(PREFIX)/bin`.
- `make install-service` installs and starts a systemd user service and seeds
  `~/.engram/.env` with mode `0600` only when absent.
- The service must run the installed binary with `~/.engram/.env` and restart
  after failure without closing tmux sessions.
- Updating a running installation requires replacing the binary and restarting
  the user service.
- `make uninstall-service` removes the systemd user unit, and `make uninstall`
  removes the binary. Neither operation deletes tmux sessions, configuration,
  state, logs, or `/tmp/engram` artifacts.
- Login lingering is an explicit optional host-policy choice, not an automatic
  installation step.

## macOS

- macOS must compile and support build, install, diagnostics, foreground run,
  and binary uninstall paths.
- Engram does not provide launchd integration.
- `make install-service` and `make uninstall-service` are Linux-only because
  they require `systemctl`.
- A user-authored LaunchAgent is outside the supported service lifecycle.

## Diagnostics

- `/status` shows version, uptime, session count, anchor mode, snapshot renderer
  capability, state path, audit path, attachment path, free `/tmp` space, poll
  time, and whether Haiku is enabled.
- `/logs` uploads a bounded recent redacted audit log tail as an attachment,
  spanning the current and rotated audit files when necessary.
- `engram version` reports binary version, commit, date, and Go version locally.
- `engram preflight`, `engram status`, and `engram dry-start` validate the local
  service surface without calling Telegram, Anthropic, or starting polling.
- `dry-start` may create and open local state; `preflight` must not.

## Local Quality Gates

- `make check` is the default local quality gate before pushing.
- CI must run build, tests, architecture checks, public-readiness checks, and
  tracked-file secret checks.
- Local quality gates must also enforce stdlib-only dependencies and docs
  freshness.

## Scope

Engram supports Go 1.22 or newer and should stay Go-stdlib-only. New
dependencies require an explicit requirement update.
