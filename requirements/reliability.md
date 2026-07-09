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
- State writes use a temporary file in the state directory with mode `0600`.
  Engram writes and syncs that file, closes it, atomically renames it over the
  previous state, and then syncs the parent directory. A failure before rename
  leaves the previous state in place and removes the temporary file. A failure
  after rename can leave the new state visible while reporting an error because
  its rename durability is uncertain; callers must treat every save error as a
  persistence failure rather than retrying the associated external action.
- On Linux, failure to sync either the file or parent directory fails the save.
  Subject to the filesystem and storage device honoring `fsync`, a successful
  save survives process failure and sudden power loss. On Darwin, Go's standard
  library uses `F_FULLFSYNC` for regular files and falls back to `fsync` when the
  filesystem does not support it. Engram also attempts to sync the parent
  directory, but Darwin filesystems may reject directory sync with `EINVAL` or
  `ENOTSUP`; only those errors are best effort. Rename remains atomic for process
  crashes, but sudden power loss can lose the latest rename when directory sync
  is unsupported.
- Telegram update offsets are durably accepted before message handling and then
  recorded again with the final handler status. This gives tmux input
  at-most-once delivery after a crash, avoiding surprise replayed shell
  commands at the cost of possibly dropping the in-flight Telegram update.
- The update journal retains the newest 200 accepted and handled/skipped update
  states so recent polling behavior remains inspectable after restart.
- Processed Telegram messages must still be tracked to avoid duplicate handling
  when Telegram or the process retries before the offset is durably advanced.
  The newest 2,000 message keys are retained. The existing schema stores boolean
  values without timestamps, so pruning uses the Telegram message ID encoded in
  each key rather than adding an age field or changing the schema.
- Attachment metadata retains the newest 200 records. Attachment bypasses drop
  expired and consumed records and retain the newest 100 active records. The
  terminal registry retains at most 200 records, preferring running and idle
  sessions and then the most recently updated terminal records. These limits
  bound state growth; they do not delete attachment files or tmux sessions.
- Raw terminal captures are not written to state. Their hashes and derived
  summaries remain persisted, and the current raw capture may remain in process
  memory until restart or pruning.
- If the state file is corrupt, Engram must preserve a timestamped corrupt
  backup and durably create a fresh state file. Legacy JSON remains readable;
  absent fields receive defaults, and legacy raw captures are omitted from the
  next saved file.
- A lock keyed by Telegram settings prevents duplicate pollers.

## Degradation

- If Haiku fails, reuse the last summary when possible and mark it stale.
- If Telegram edit fails, send a replacement anchor and update state.
- If HTML formatting fails, fall back to plain text.
- If tmux target capture fails, mark the session lost and stop pretending it is
  current.
