# Upstream Signal Requirements

An Engram-managed terminal may contain another Engram, a container, or any
other nested environment. A process inside it can ask for the parent user's
attention through the terminal it already controls. Engram does not create a
second transport or model a deployment hierarchy.

## Model

- An upstream signal is a bounded, visible terminal record. The emitter also
  writes the terminal bell (`BEL`) as a hint to tmux and attached clients, but
  observation never depends on bell state.
- Parent and child describe where a signal is observed, not persistent Engram
  roles. Engram stores no ancestry, topology, child credentials, or remote
  endpoint.
- Signals may cross any number of nested environments when their terminal
  output reaches a pane already tracked by the outer Engram. Each observer
  treats the signal as ordinary terminal activity at its own boundary.
- Engram deliberately amplifies a recognized terminal record into a
  parent-authenticated Telegram notification and reply alias. This does not
  reveal parent credentials to the emitter, but it does give any pane writer a
  bounded way to request the user's attention.

## Emission

- `engram signal <message>` writes `BEL`, establishes a new physical row with
  explicit CRLF, and writes one UTF-8 line beginning with the stable literal
  prefix `[engram:upstream] ` to its controlling terminal.
- Each record contains a random 128-bit lowercase hexadecimal ID between the
  prefix and payload. The ID is framing for bounded deduplication, not identity
  or authentication, and is omitted from the Telegram notification.
- The message is normalized to one line, strips terminal control characters,
  and is capped at 1 KiB after UTF-8 validation. Empty messages are rejected.
- Observation admits zero through eight leading ASCII spaces before the exact
  prefix so terminal hosts such as Codex may present command output as an
  indented block. A valid indented record may collect a bounded run of
  contiguous rows with exactly the same indentation when that host physically
  wraps its payload; those rows are removed from guide evidence with the record.
  Column-zero records remain one line. Deeper indentation, tabs, altered
  prefixes, and malformed record IDs are not signals. This tolerance is
  presentation framing, not authentication; any pane writer can already forge
  a valid record.
- The command makes no network request and reads no Engram service state. If no
  controlling terminal is available, it exits nonzero without attempting a
  fallback transport.
- Every intervening runtime must preserve a controlling PTY. Detached services,
  cron/CI jobs, `setsid`, ordinary `docker exec`, and SSH without `-t` do not
  satisfy this requirement even when their stdout is otherwise redirected to a
  tracked pane.
- A process may emit the same wire form directly. The CLI is convenience, not
  a privileged or authenticated protocol.

## Observation And Delivery

- `CaptureStyled` obtains physical ANSI rows and a joined logical-text view over
  the same bottom-bounded coordinates in one tmux command batch. The parent
  scans that joined view for the newest recognized record. It opens no listener,
  socket, pipe, port, or shared state directory.
- Where tmux exposes a window bell flag, Engram may use it to accelerate a
  capture. Bell state is window-scoped, may be absent for a selected window,
  and must not be cleared through a window-selection side effect.
- A recognized upstream record immediately attempts one concise Telegram
  notification containing its normalized semantic payload after normal output
  redaction. Anchor rendering continues independently; the guide or Chromium
  latency and failure must not suppress the attention attempt.
- Engram normally replies the notification to the canonical anchor. If Telegram
  reports that reply target missing, Engram sends a standalone notification,
  records it as the current reply alias, and schedules canonical-anchor
  recovery.
- Engram removes recognized records from guide input and deterministic
  path/URL extraction. A literal snapshot may still show the record as part of
  the exact terminal frame under the snapshot privacy boundary.
- The newest upstream reply is a reply alias for the observed parent pane.
  Replying to it sends input to that pane through the normal identity checks.
  Publishing a newer upstream reply makes its predecessor stale; stale replies
  never route input and receive the standard concise stale-reply response.
- A bell without a recognized record may refresh the anchor but creates no
  upstream reply. A recognized record remains sufficient when tmux does not
  retain or expose its accompanying bell.
- A bounded per-terminal set of record IDs deduplicates repeated observation of
  the same terminal record while allowing distinct records with identical
  payloads. Engram does not mutate tmux selection to clear alert flags.
- Signal notifications are coalesced per pane to at most one every ten seconds.
  The first signal in an interval may notify immediately; later signals remain
  visible in the refreshed terminal frame and may notify after the interval
  only if a distinct newest record still requires delivery.

## Reply Semantics

- A reply reaches the outer tmux pane, not a named inner Engram or inner tmux
  session. When the nested environment is foregrounded, normal terminal input
  naturally carries it inward.
- Engram does not promise deterministic routing through an inner shell, tmux
  client, multiplexer, full-screen program, or process that is no longer in the
  foreground. The canonical anchor remains the truthful routing boundary.
- Telegram authorization, pane/window identity validation, slash escaping, and
  Enter behavior are identical to every other current reply alias.

## Security And Privacy

- Telegram credentials, allowed user identifiers, model-provider credentials,
  parent state, and the parent tmux socket must not be passed or mounted into a
  child for signaling.
- Signal payload is untrusted terminal text. Its textual notification and audit
  record receive best-effort credential redaction, and it must never be parsed
  as a command, callback, path, URL to fetch, or proof of identity. Exact
  snapshot pixels remain unredacted like the rest of that terminal frame.
- The marker is framing, not authentication. Any process able to write to the
  tracked pane can emit it. Notifications identify themselves as terminal
  signals; users must treat their payload as untrusted pane-authored text.
- Audit records may retain a bounded redacted signal payload and delivery
  result, but never additional child identity inferred from terminal text.

## Failure And Recovery

- Signaling is best effort. A signal can be missed if it leaves tmux history
  before capture, the process cannot reach the controlling terminal, or the
  parent service is unavailable long enough for tmux's bell state to be lost.
- Telegram failure leaves the terminal record intact and auditable. It must not
  block tmux input, polling, or later anchor refreshes.
- Telegram delivery and local persistence cannot be atomic. A crash or lost API
  response after Telegram accepts a notification can produce a duplicate after
  restart. A definite state-persistence failure triggers a best-effort
  compensating deletion, but cannot close the process-crash window.
- Successfully persisted record IDs suppress repeat observation across restart.
  The retained ID set stays bounded per terminal.
- Signal capture or delivery failure never marks a healthy pane lost.

## Non-Goals

- An Engram-to-Engram API, control plane, hierarchy, or discovery protocol.
- Giving a nested environment direct access to the user's Telegram bot.
- Mapping inner tmux sessions to outer Telegram anchors.
- Guaranteed delivery, acknowledgements, retries between Engram instances, or
  background signaling without a controlling terminal.
- Forwarding attachments, files, commands, or arbitrary structured messages.
- Treating arbitrary terminal bells as trusted or user-visible messages.

If future work requires guaranteed delivery, attachments, acknowledgements, or
direct routing to inner sessions, it must justify a separate transport as a new
capability rather than silently expanding this terminal signal.
