# Engram Requirements Index

Status: draft but binding for implementation.

Engram keeps requirements small and executable. Each document states runtime
contracts that should either be tested directly or checked by `make check`.
The requirements documents are the binding source of truth.

## Foundation

1. [`telegram.md`](telegram.md) - Telegram command, callback, formatting, and delivery contracts.
2. [`tmux.md`](tmux.md) - tmux target selection, attachment, input, capture, and close behavior.
3. [`reliability.md`](reliability.md) - failure handling, audit evidence, retry/degradation rules.
4. [`security.md`](security.md) - single-user admission, secrets, filesystem, and tmux risk boundaries.
5. [`operations.md`](operations.md) - service lifecycle, systemd, logs, state, and diagnostics.
6. [`upstream-signals.md`](upstream-signals.md) - terminal-native attention signals from nested environments.

## Executable Checks

- `make test` runs unit and contract tests.
- `make architecture` checks package boundaries and required requirement files.
- `make public-readiness` checks public-facing repository hygiene.
- `make secrets` scans tracked files for likely live secrets.
- `make check` runs the full local quality gate.
