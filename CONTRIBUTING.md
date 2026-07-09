# Contributing

Engram is intentionally small, single-user, and Go-stdlib-only. Keep changes
focused on making tmux easier to operate from a private Telegram DM.

## Before Opening A Change

Run the full gate:

```sh
make check
```

For documentation-only changes, at minimum run:

```sh
make public-readiness
make docs-freshness
make secrets
```

## Change Guidance

- Update command metadata when changing commands.
- Update README, requirements, privacy guidance, and `.env.example` when
  changing configuration, external data flow, storage, or service behavior.
- Add focused tests for Telegram payloads, tmux behavior, state migration,
  attachment handling, and security boundaries.
- Keep Linux systemd behavior distinct from macOS manual operation.
- Do not add third-party Go dependencies without an explicit requirements
  update and design review.
- Keep terminology specific to Engram, Telegram, tmux, and Haiku.

## Sensitive Evidence

Never commit or attach live `.env` files, bot tokens, API keys, local state,
audit logs, terminal captures, attachments, private paths, or credentials.
Redaction is best effort, so inspect every issue attachment and test fixture
manually. Use synthetic values in tests and reports.

Security issues must use the private reporting route in
[`SECURITY.md`](SECURITY.md), not a public issue.
