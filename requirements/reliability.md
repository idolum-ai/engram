# Reliability Requirements

Engram is a long-running service. Failure must be visible and recoverable.

## Principles

- Never silently pretend a Telegram or tmux action succeeded.
- Preserve state coherently before attempting cosmetic updates.
- Keep polling alive after transient Telegram errors.
- Keep tmux sessions alive when Haiku or Telegram delivery fails.
- Prefer truthful degraded summaries over missing anchors.

## Audit

The audit JSONL must record important machine facts:

- service start and stop
- Telegram command receipt
- Telegram send/edit failures
- tmux send success and failure
- Haiku failures
- command registration success and failure

Secrets must be redacted before logs are uploaded through `/logs`.

## State

- State lives under `~/.engram` by default.
- State writes must be atomic.
- Processed Telegram messages must be tracked to keep polling idempotent.
- A lock keyed by Telegram settings prevents duplicate pollers.

## Degradation

- If Haiku fails, reuse the last summary when possible and mark it stale.
- If Telegram edit fails, send a replacement anchor and update state.
- If HTML formatting fails, fall back to plain text.
- If tmux target capture fails, mark the session lost and stop pretending it is
  current.
