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
- state persistence failures that would otherwise hide lost progress
- command registration success and failure

Secrets must be redacted before logs are uploaded through `/logs`. `/logs`
exports a bounded recent tail, not an unbounded full audit file.

## State

- State lives under `~/.engram` by default.
- State writes must be atomic.
- Telegram update offsets are durably accepted before message handling and then
  recorded again with the final handler status. This gives tmux input
  at-most-once delivery after a crash, avoiding surprise replayed shell
  commands at the cost of possibly dropping the in-flight Telegram update.
- A bounded update journal records accepted and handled/skipped update states so
  recent polling behavior remains inspectable after restart.
- Processed Telegram messages must still be tracked to avoid duplicate handling
  when Telegram or the process retries before the offset is durably advanced.
- If the state file is corrupt, Engram must preserve a timestamped corrupt
  backup and start with a fresh state file.
- A lock keyed by Telegram settings prevents duplicate pollers.

## Degradation

- If Haiku fails, reuse the last summary when possible and mark it stale.
- If Telegram edit fails, send a replacement anchor and update state.
- If HTML formatting fails, fall back to plain text.
- If tmux target capture fails, mark the session lost and stop pretending it is
  current.
