# Operations Requirements

Engram should be simple to install, inspect, and recover.

## Installation

- `make build` builds the binary.
- `make install` installs to `$(PREFIX)/bin`.
- `make install-service` installs and starts a user systemd service.
- `make install-service` must seed `~/.engram/.env` with `0600` permissions.

## Diagnostics

- `/status` shows version, uptime, session count, state path, audit path,
  attachment path, free `/tmp` space, poll time, and Haiku status.
- `/logs` uploads a bounded recent redacted audit log tail as an attachment.
- `/version` reports binary version, commit, date, and Go version.
- `engram preflight`, `engram status`, and `engram dry-start` validate the
  local service surface without calling Telegram, Anthropic, or starting
  polling.

## Local Quality Gates

- `make check` is the default local quality gate before pushing.
- CI must run build, tests, architecture checks, public-readiness checks, and
  tracked-file secret checks.
- Local quality gates must also enforce stdlib-only dependencies and docs
  freshness.

## Scope

Engram should stay Go-stdlib-only.
New dependencies require an explicit requirement update.
