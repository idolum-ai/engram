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
- A signal grants no capability beyond writing to the terminal the emitting
  process already controls.

## Emission

- `engram signal <message>` writes `BEL` and one UTF-8 line
  beginning with the stable literal prefix `[engram:upstream] ` to its
  controlling terminal.
- The message is normalized to one line, strips terminal control characters,
  and is capped at 1 KiB after UTF-8 validation. Empty messages are rejected.
- The command makes no network request and reads no Engram service state. If no
  controlling terminal is available, it exits nonzero without attempting a
  fallback transport.
- A process may emit the same wire form directly. The CLI is convenience, not
  a privileged or authenticated protocol.

## Observation And Delivery

- The parent scans its existing bounded `CaptureStyled` observations for the
  newest recognized record. It opens no listener, socket, pipe, port, or shared
  state directory.
- Where tmux exposes a window bell flag, Engram may use it to accelerate a
  capture. Bell state is window-scoped, may be absent for a selected window,
  and must not be cleared through a window-selection side effect.
- A recognized upstream record in that capture refreshes the canonical anchor
  and produces one concise Telegram reply to that anchor containing the exact
  normalized payload after normal output redaction. Engram does not interpret,
  summarize, execute, or endorse the payload.
- Engram removes recognized records from Haiku input and deterministic
  path/URL extraction. A literal snapshot may still show the record as part of
  the exact terminal frame under the snapshot privacy boundary.
- The newest upstream reply is a reply alias for the observed parent pane.
  Replying to it sends input to that pane through the normal identity checks.
  Publishing a newer upstream reply makes its predecessor stale; stale replies
  never route input and receive the standard concise stale-reply response.
- A bell without a recognized record may refresh the anchor but creates no
  upstream reply. A recognized record remains sufficient when tmux does not
  retain or expose its accompanying bell.
- Capture hashes and the normalized record deduplicate repeated observation of
  the same signal. Engram does not mutate tmux selection to clear alert flags.
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

- Telegram credentials, allowed user identifiers, Anthropic credentials,
  parent state, and the parent tmux socket must not be passed or mounted into a
  child for signaling.
- Signal payload is untrusted terminal text. Its textual notification and audit
  record receive best-effort credential redaction, and it must never be parsed
  as a command, callback, path, URL to fetch, or proof of identity. Exact
  snapshot pixels remain unredacted like the rest of that terminal frame.
- The marker is framing, not authentication. Any process able to write to the
  tracked pane can emit it because that process can already influence what the
  user sees and types into that pane.
- Audit records may retain a bounded redacted signal payload and delivery
  result, but never additional child identity inferred from terminal text.

## Failure And Recovery

- Signaling is best effort. A signal can be missed if it leaves tmux history
  before capture, the process cannot reach the controlling terminal, or the
  parent service is unavailable long enough for tmux's bell state to be lost.
- Telegram failure leaves the terminal record intact and auditable. It must not
  block tmux input, polling, or later anchor refreshes.
- A restart may redeliver the newest visible signal at most once. Persistent
  deduplication state must stay bounded and scoped to its terminal record.
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
