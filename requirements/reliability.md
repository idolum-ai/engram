# Reliability Requirements

Engram is a long-running service. Failure must be visible and recoverable.

## Principles

- Never silently pretend a Telegram or tmux action succeeded.
- Preserve state coherently before attempting cosmetic updates.
- Keep polling alive after transient Telegram errors.
- Keep tmux sessions alive when Haiku, Chromium, or Telegram delivery fails.
- Keep polling and tmux input responsive while terminal images render and upload.
- Keep polling and tmux input responsive while upstream signals refresh or notify.
- Prefer truthful degraded presentation over missing anchors.

## Audit

The audit JSONL must record important machine facts:

- service start and stop
- Telegram command receipt
- Telegram send/edit failures
- tmux send success and failure
- Haiku failures
- upstream-signal observation and delivery outcomes
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
  Session updates roll in-memory state back after a failure before replacement;
  after replacement they retain the visible new state and report uncertain
  durability so memory and the current state file do not diverge.
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
- Audit records are capped at 64 KiB. `audit.jsonl` rotates before exceeding
  4 MiB and retains one predecessor capped at the same size. Rotation preserves
  complete recent JSONL records, including when adopting an oversized legacy
  log.
- Processed Telegram messages must still be tracked to avoid duplicate handling
  when Telegram or the process retries before the offset is durably advanced.
  The newest 2,000 message keys are retained. The existing schema stores boolean
  values without timestamps, so pruning uses the Telegram message ID encoded in
  each key rather than adding an age field or changing the schema.
- Attachment metadata retains the newest 200 records. Attachment bypasses drop
  expired and consumed records and retain the newest 100 active records. The
  terminal registry retains at most 200 records, preferring running sessions
  and then the most recently updated terminal records. These limits
  bound state growth; they do not delete attachment files or tmux sessions.
- Raw terminal captures are not written to state. Their hashes and bounded
  conversational renderings may remain persisted; current raw capture may
  remain in process memory until restart or pruning.
- Session state persists only runtime facts used for recovery or rendering.
  Legacy write-only fields are ignored and disappear on the next save. Legacy
  terminal states other than `running`, `lost`, and `closed` normalize to
  `lost`, where immutable-identity reattachment can recover them safely.
- Session state persists the canonical anchor, at most one predecessor awaiting
  retirement, each anchor's text or snapshot format, and known/unknown Telegram
  pin state. Restart resets pin knowledge and reconciles presentation without
  discarding canonical ownership.
- If the state file is corrupt, Engram must preserve a timestamped corrupt
  backup and durably create a fresh state file. Legacy JSON remains readable;
  absent fields receive defaults, and legacy raw captures are omitted from the
  next saved file.
- A state schema newer than the running binary supports must fail open without
  rewriting or down-stamping the file.
- State schema v8 persists `anchor_mode`, the latest conversational, snapshot,
  and upstream-signal reply IDs, upstream deduplication facts, and a bounded
  stale-alias set used only to reject confusing replies. It binds each watch to
  a random tmux server incarnation so reused pane/window IDs after a server
  restart cannot silently gain authority. Legacy watches without that identity
  require an explicit `/attach` from `/sessions`; reattachment changes their
  origin to attached, so Engram cannot close a newly adopted window. Valid
  legacy state otherwise migrates forward; retired interpretation fields are
  ignored and disappear on save.
- A lock keyed by Telegram settings prevents duplicate pollers.
- Upstream-signal record-ID deduplication is bounded per terminal. Successful
  persistence suppresses a visible record across restart; a crash between
  Telegram acceptance and persistence can duplicate it. Signal failures never
  mark a healthy pane lost.
- The latest upstream-signal reply alias persists with the terminal registry so
  reply routing remains truthful after restart. Superseded aliases join the
  existing bounded stale-alias set.

## Degradation

- If Haiku fails, retain the canonical anchor and report the failure without
  inventing a conversational rendering.
- A failed mode-migration send leaves the old anchor canonical. A failed
  predecessor retirement or pin transition remains eligible for retry.
- If Telegram reports an anchor missing or uneditable, send a replacement and
  update state. Rate limits do not trigger replacement amplification, and
  unchanged edits count as success.
- Chromium readiness controls both snapshot startup and whether guide anchors
  expose `🖼️` or allow `/mode snapshot`. A later capture, render,
  or upload failure is audited and leaves the canonical anchor and tmux session
  unchanged for retry.
- Snapshot refreshes hash styled capture and metadata before invoking Chromium,
  coalesce per session, use bounded capture/render concurrency, and edit
  automatically no more than once every ten seconds when content changed. A
  failed automatic migration attempt is also throttled before capture and
  rendering, preventing retry amplification. A manual refresh may render the
  same capture immediately.
- An acknowledged one-off conversational request ends with either its reply or
  one bounded failure notice. A concurrent anchor or mode change supersedes the
  request visibly rather than delivering it against a different live anchor.
- Entering `snapshot` mode converts a text anchor to photo media in place so its
  message identity, pin, reply routing, and controls remain canonical. Returning
  to `guide` mode uses one persisted send-before-retire rotation because Telegram
  cannot convert a media message back to text in place.
- A session may persist at most one retiring predecessor. Engram must finish or
  durably retain that retirement before mode migration, media replacement, or
  any other operation can create another predecessor.
- If Telegram rejects HTML entity parsing, fall back once to plain text. Other
  API failures retain their typed outcome.
- Cancellation, timeout, or a generic tmux capture failure does not prove that
  a pane disappeared. Mark a session lost only when tmux explicitly reports the
  pane missing or its immutable pane/window identity no longer matches.
- Malformed tmux metadata fails the current operation without synthesizing
  missing fields. In particular, `/attach` succeeds only after complete
  immutable session, window, and pane identities have been parsed.
- A later user action may restore a lost session when the same immutable pane
  and window identity validates successfully. Recovery must be audited and
  followed by a fresh capture.
- Upstream-signal notifications are coalesced to at most one per pane every ten
  seconds. A retained tmux bell may accelerate capture, but signal discovery
  remains polling-based and does not bypass bounded rendering work or amplify
  Telegram retries.
- A Telegram `retry_after` that outlives the client's bounded retry is retained
  in memory before persistence is attempted, then persisted per terminal. A
  pre-replacement state-write failure must not cause the running process to
  immediately repeat the request.
- The signal delivery timestamp is recorded after Telegram succeeds, so a
  delayed retry cannot shorten the ten-second interval between successful
  notifications.
