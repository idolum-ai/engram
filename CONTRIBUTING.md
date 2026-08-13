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
- Walk the affected row in [`docs/product-surface.md`](docs/product-surface.md)
  from entry through authority, effect, evidence, and recovery. Add or revise a
  row when a change creates a new user journey.
- Update the binding requirement, implementation, focused test, and the one
  owning user document when behavior changes. Link from the README instead of
  copying detailed reference material into it.
- Update [`docs/configuration.md`](docs/configuration.md) and `.env.example`
  together when changing supported configuration.
- Update [`docs/data-flow.md`](docs/data-flow.md), `SECURITY.md`, and
  `requirements/security.md` when changing external transfer, local sensitive
  storage, authority, or execution boundaries.
- Add focused tests for Telegram payloads, tmux behavior, state migration,
  attachment handling, and security boundaries.
- Keep Linux systemd activation distinct from the macOS LaunchAgent's explicit
  activation.
- Do not add third-party Go dependencies without an explicit requirements
  update and design review.
- Use the terms in [`docs/conceptual-model.md`](docs/conceptual-model.md).
  Distinguish a user-facing session from its watch and tmux pane; distinguish
  configuration from availability; and distinguish approval, access, and
  outcome.
- Write documentation in simple English. Keep it professional, deadpan, and
  concise. Use technical terms only when they make the behavior more exact.

## Sensitive Evidence

Never commit or attach live `.env` files, bot tokens, API keys, local state,
audit logs, terminal captures, attachments, private paths, or credentials.
Redaction is best effort, so inspect every issue attachment and test fixture
manually. Use synthetic values in tests and reports.

Security issues must use the private reporting route in
[`SECURITY.md`](SECURITY.md), not a public issue.

## Releases

Ordinary contributions target `main` and must not create tags or GitHub Release
assets. Maintainer releases use a short-lived `release/vX.Y.Z` branch, a reviewed
pull request into protected `main`, a matching changelog section, and guarded
automation. See [`docs/release-strategy.md`](docs/release-strategy.md) for the
complete procedure and recovery rules.
