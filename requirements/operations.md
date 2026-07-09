# Operations Requirements

Engram should be simple to install, inspect, and recover.

## Installation

- `make build` builds the binary.
- `make install` installs to `$(PREFIX)/bin`.
- `make install-service` installs and starts a user systemd service.

## Diagnostics

- `/status` shows version, uptime, session count, state path, audit path,
  attachment path, free `/tmp` space, poll time, and Haiku status.
- `/logs` uploads redacted audit logs as an attachment.
- `/version` reports binary version, commit, date, and Go version.

## Local Quality Gates

- `make check` is the default local quality gate before pushing.
- CI must run build, tests, architecture checks, public-readiness checks, and
  tracked-file secret checks.

## Scope

Engram should stay Go-stdlib-only.
New dependencies require an explicit requirement update.
