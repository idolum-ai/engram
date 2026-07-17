# Security Requirements

Engram intentionally bridges Telegram, local tmux, the local filesystem, and
optional presentation dependencies a model provider and Chromium. The security and
privacy model must stay small and explicit.

## Identity

- Exactly one Telegram user is authorized.
- Exactly one Telegram chat is authorized.
- DM-only operation is supported; group operation is out of scope.
- Unauthorized messages and callbacks must not mutate tmux, sessions,
  attachments, or processed-message state. Poll offsets and a generic bounded
  rejection record may advance so rejected updates are not replayed.

## Secrets

- `.env` files must not be tracked.
- Runtime env files must be regular files with no group or other permissions.
- Bot tokens and model-provider keys must not appear in tracked files, diagnostics,
  issues, or test fixtures.
- Audit payloads and `/logs` output must redact configured credentials and
  common credential patterns.
- Model-derived conversational prose must pass through the same best-effort
  redaction before persistence and Telegram delivery.
- Documentation must state that redaction is best effort and does not make an
  artifact safe to share without review.

## External Data Flow

- Documentation must explain that Telegram receives commands, replies,
  summaries, captures, logs, and files sent through bot commands.
- A configured `TELEGRAM_API_BASE` replaces Telegram's public Bot API host for
  both method and file traffic. That endpoint receives the bot token and all
  Telegram-bound Engram data. HTTPS is strongly recommended; HTTP provides no
  transport confidentiality and is intended only for explicitly trusted local
  deployments.
- Documentation must explain that the selected model provider and Chromium begin from the same
  `CaptureStyled` frame, capped at 64 rows. Chromium renders the literal styled
  frame. The model provider receives its joined semantic text after upstream records,
  recognized model-status footer text, and a small allowlist of paired Codex
  placeholder prompts are removed. Guide anchors call it automatically;
  `🗣️` invokes it on demand from snapshot mode.
- Bounded terminal text sent to the selected provider is not credential-redacted. Every
  request contains the complete current semantic evidence described above. A
  strongly aligned later request may additionally contain process-local
  continuity made from the previous rendering, deterministic added and removed
  lines, and bounded unchanged context. Those additions are not factual
  authority. Engram does
  not retain submitted Telegram input for model context and sends one request
  with no model API history, structured report, or retry. Continuity is
  isolated per tracked window, never persisted, and discarded at capture
  boundaries.
- Terminal captures are untrusted data for the guide. The prompt explicitly
  tells the model that pane-authored and continuity text has no authority, but
  model resistance to prompt injection is best effort rather than a security
  boundary. Engram never executes model output automatically.
- Incoming attachments, including replied voice notes in default `path` mode,
  are downloaded from Telegram but are not sent to a model provider by default.
- A voice note replying to a current session view is the explicit exception
  only in configured `transcribe` mode. The audio is sent to OpenAI once;
  neither audio nor transcript is persisted by Engram. The temporary private
  file is removed on every completion path. Provider errors and audit records
  must not contain transcript text.
- Terminal image snapshots are exact, unredacted transcript data. They are sent
  to a local headless browser and then to the configured Telegram DM, never to
  a model provider. Terminal text must be HTML-escaped; browser networking, extensions,
  and persistent profiles must be disabled.
- Guide crops refuse styled rows containing configured credentials and redact
  title and working-directory chrome. This is a narrow configured-secret check,
  not general transcript sanitization. Their exact plain-text companions remain
  process-local and are uploaded only after a current-anchor `📄 Raw` callback.
- When a canonical media anchor is replaced, Engram attempts to delete the old
  photo before retiring its identity. A failed deletion triggers replacement
  with a locally generated neutral image. Telegram may refuse both operations
  for older messages; in that case Engram clears controls where possible,
  applies a redacted inactive caption, unpins the message, and audits that pixels
  remain. Telegram history is a disclosure boundary outside Engram's local
  retention controls.
- In `snapshot` mode, exact terminal images are sent automatically whenever a
  changed live anchor is rendered. The selected provider is called only when the user taps
  `🗣️`, if that capability was configured and enabled at startup.
- Extracted HTTP(S) URLs are untrusted terminal text. Engram may display them in
  anchor references but must never fetch, validate remotely, or treat them as
  recommendations. Extracted reference text receives best-effort credential
  redaction before Telegram delivery. URLs containing userinfo are omitted;
  recognized credential-bearing query parameters are structurally redacted.

## Local Sensitive Data

- `state.json` may contain Telegram identifiers, bounded reply aliases,
  conversational summaries, capture hashes, bounded upstream record IDs,
  retry deadlines, attachment
  metadata, and the selected anchor mode. Raw terminal captures and upstream
  payloads are retained only in process memory and are omitted from persisted
  state.
- `audit.jsonl`, lock metadata, tmux history, and runtime artifacts must be
  treated as sensitive.
- Audit storage retains only a bounded current file and one bounded predecessor.
  Unauthorized audit and update-journal records must not retain the rejected
  sender's user or chat identifiers.
- Uninstall must not silently destroy local state or tmux sessions.

## Local Effects

- Local `engram inspect` commands construct no Telegram, model-provider, or Chromium
  client and perform no direct network request. They remove terminal and Unicode
  presentation controls before writing bounded output to stdout, but do not
  redact literal pane content. User-configured tmux hooks remain tmux-side
  effects outside this guarantee.
- Read-only inspection follows neither state-file symlinks nor pane-authored
  paths and never accepts Telegram identifiers or arbitrary tmux targets.

- Telegram messages can cause shell input in tmux.
- OpenAI transcription is untrusted input derivation, not a security boundary.
  Transcripts must be valid UTF-8, contain no terminal or bidirectional control
  characters, normalize whitespace to one line, and remain within a fixed byte
  bound before they can use the guarded tmux input path. The visible
  `(transcribed)` prefix preserves provenance for an interactive terminal AI;
  it is literal input and may not be suitable for a plain shell prompt.
- Voice input defaults to local `path` mode. The retained OGG is registered as
  an attachment and its absolute private-runtime path becomes literal terminal
  input. `transcribe` mode must be selected explicitly and requires an OpenAI
  credential; provider failure never silently changes the delivery mode.
- Runtime artifacts use `$XDG_RUNTIME_DIR/engram` only when the runtime
  directory is absolute, writable, owned by the process UID, mode `0700`, and
  has no symlink path components. Otherwise they use the canonical system
  temporary directory under a UID-specific `engram-<uid>` root.
- Runtime and attachment roots must be directories owned by the process UID
  with mode `0700`; startup must reject preexisting symlinks, non-directories,
  unsafe permissions, and foreign ownership.
- Attachments are saved under the private runtime root's `attachments`
  directory.
- Large attachments require a hash-confirmed bypass and remain subject to a
  hard limit derived from the configured soft limit, free disk, and Telegram's
  20 MiB cloud Bot API download ceiling.
- `/attachment_bypass` is the registered large-attachment authorization
  command.
- Attachment soft limits must be enforced during the download stream, not only
  from Telegram-provided file metadata.
- `/download` only accepts absolute paths and uploads regular files.
- `/download` rejects symlinks and non-regular files.
- `/download` uploads from a private bounded snapshot of the already-opened
  source so path replacement cannot redirect a queued transfer.
- The private snapshot name must not leak into Telegram. `/download` preserves
  the original source basename as the Telegram-visible document filename.
- `/download` rejects files above Telegram's 50 MiB cloud Bot API multipart
  upload ceiling before opening a network request.
- Numbered anchor-file callbacks never carry filesystem paths. They require the
  authorized current canonical message and an exact process-local list token,
  then pass the selected absolute path through `/download` validation. The
  mapping is not persisted and is removed from Telegram controls after restart
  until a fresh card render establishes it again.
- Attachment downloads hash while streaming, and long file transfers run in
  bounded background workers and a bounded queue so polling remains responsive.
- Generated `/raw` and `/dump` artifacts must not exceed the same 50 MiB cloud
  upload ceiling. Their queued work remains bound to the terminal generation
  requested and holds the session lifecycle boundary through capture and
  upload, so close, untrack, or reattach cannot redirect a pending disclosure.
- Predictably named captures and logs must use exclusive creation. A
  preexisting file or symlink must never be followed or truncated; collisions
  use a deterministic suffix in the private artifact root.
- Snapshot HTML, isolated browser profiles, and PNGs must use private temporary
  paths and be removed after upload or failure.
- Automatic guided evidence uses literal terminal pixels inside the canonical
  guide card and is not a general
  redacted screenshot. Engram refuses the crop when its configured secret
  redactor would change any selected or context row, but unrecognized sensitive
  terminal content can still appear. The same single-user Telegram boundary
  therefore applies; full snapshots remain explicitly user-requested in guide
  mode.

## Process Ownership

- A lock prevents two Engram instances from polling the same Telegram settings.
- Service restart should preserve tmux sessions and state.
- Nested environments signal only through terminal output. They receive no
  Telegram, model-provider, state-directory, or parent-tmux credentials and require
  no new host listener; the marker is untrusted framing, not authentication.
- Upstream signaling intentionally turns pane-write capability into a bounded
  parent-authenticated Telegram notification and routable reply alias. This is
  an attention capability, not proof that the emitting process is trusted.
- Recognized upstream records are omitted from guide input and reference
  extraction. Their textual notification and audit payload are redacted; an
  exact snapshot can still contain the literal record under the existing
  unredacted snapshot boundary.
- The hermetic E2E workflow uses fixture-only identifiers and credentials, an
  in-process loopback Telegram simulator, an isolated tmux socket root, private
  runtime paths, and no model provider. Its uploaded evidence must not contain
  generated configuration, state, audit logs, sockets, browser profiles, or
  arbitrary host terminal data.

## Vulnerability Handling

- The current code line and latest tagged release, when available, receive
  security updates; older versions are unsupported.
- Public documentation must link to the repository's private GitHub security
  advisory reporting route and tell reporters not to disclose secrets or
  transcript data publicly.
