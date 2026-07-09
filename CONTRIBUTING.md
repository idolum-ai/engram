# Contributing

Engram is intentionally small and Go-stdlib-only.

Before opening a change:

```sh
make check
```

Keep changes scoped:

- Update command metadata when changing commands.
- Update requirements or README when changing behavior.
- Add tests for Telegram payloads, tmux behavior, state migration, and security
  boundaries.
- Do not add third-party dependencies without an explicit requirements update.
- Do not commit live `.env`, state, logs, attachments, or credentials.
