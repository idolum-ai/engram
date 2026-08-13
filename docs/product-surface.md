# Product Surface

Status: descriptive. The files under
[`requirements/`](../requirements/INDEX.md) remain the binding runtime contract.

Engram has one remote control surface, a small set of local diagnostics, and one
optional local capability broker. This map joins each user journey to its
authority boundary, visible evidence, recovery path, and binding contract.

## Product Boundary

- Telegram is the remote product surface. One configured user and one private
  chat can act.
- tmux is the workspace. Its panes and history continue when Engram stops.
- A tracked session is the user-facing handle for one Engram watch. The watch
  records the immutable tmux binding and the current Telegram presentation.
- Local inspection observes Engram and tmux without becoming another control
  surface.
- GitHub access is optional. It gives one validated pane a bounded child-process
  capability after explicit approval; it does not make GitHub part of tmux or
  Telegram.

The [conceptual model](conceptual-model.md) defines the terms. The table below
shows where a user encounters them.

## Journey Map

| User goal | Entry | Authorized effect or disclosure | Success evidence | Recovery | Binding contract |
| --- | --- | --- | --- | --- | --- |
| Configure Engram | Protected `.env` | Select one user/chat, presentation, local paths, and optional providers | `engram preflight` ends with `status: ok` | Correct the named field; no remote call or state write has occurred | [Operations](../requirements/operations.md), [Security](../requirements/security.md) |
| Initialize local state | `engram dry-start` | Create and validate private local state without polling | Output names each local surface and ends with `status: ok` | Fix the reported file, ownership, permission, or lock error | [Operations](../requirements/operations.md), [Reliability](../requirements/reliability.md) |
| Start or operate the service | Foreground `engram run` or native user service | Poll Telegram and observe local tmux as the configured OS user | Telegram `/status` and native `service-status` identify the running build from their respective surfaces | Restart through the native service; tmux descendants remain | [Operations](../requirements/operations.md) |
| Create a working session | Telegram `/new <text>` | Create one tmux window and submit one shell command | A pinned session anchor appears and reflects a captured frame | `/sessions`, `/recovery`, or local inspection identifies the watch | [Telegram](../requirements/telegram.md), [tmux](../requirements/tmux.md) |
| Adopt existing work | Telegram `/attach <target>` or an attach button | Bind a watch to the exact current tmux server, window, and pane | The tracked session retains the validated immutable identity | Identity change fails closed and requires explicit reattachment | [tmux](../requirements/tmux.md), [Reliability](../requirements/reliability.md) |
| Send input | Reply, `/send`, `/text`, `/key`, or inline controls | Deliver the declared input only after binding validation | tmux receives the exact selected command, text, or key sequence | Stale routes and changed identities return a user error without input | [Telegram](../requirements/telegram.md), [tmux](../requirements/tmux.md) |
| Read current work | Guide, snapshot, `/raw`, `/dump`, or local `inspect frame` | Disclose the selected bounded frame or scrollback through the chosen surface | The result states or preserves its presentation class | Guide failure can retain literal evidence; local inspection remains available | [tmux](../requirements/tmux.md), [Security](../requirements/security.md) |
| Change presentation | Telegram `/mode`, `🖼️ View`, or `🗣️ Talk` | Render or interpret one frame through an available capability | The current anchor migrates only after replacement delivery succeeds | The prior anchor remains current on replacement failure | [Telegram](../requirements/telegram.md), [Reliability](../requirements/reliability.md) |
| Add agent history | Codex or Claude hook plus an explicit context-turn setting | Admit bounded visible history from one exactly identified active session | The compatibility card reports each independent axis | Any failed identity, binding, screen, or parser check falls back to terminal-only guidance | [Security](../requirements/security.md), [Agent compatibility](agent-compatibility.md) |
| Recover an agent session | Telegram `/recovery` or `/resume` | Create a replacement pane for the exact stored provider/session identity | The existing watch and anchor bind to the new pane | The plan remains explicit; Engram never replays an observed launch automatically | [Telegram](../requirements/telegram.md), [tmux](../requirements/tmux.md) |
| Move a file | Telegram attachment, `/download`, `/raw`, `/dump`, or template export | Transfer one bounded, validated file through Telegram | The message identifies the intended artifact without leaking private snapshot paths | Validation or queue failure leaves the source unchanged and reports the reason | [Security](../requirements/security.md), [Reliability](../requirements/reliability.md) |
| Run a GitHub command | `engram github exec` | Start one child with a validated installation token inside the approved envelope | CLI result and compact Telegram receipt report the request outcome | Denial, scope mismatch, cancellation, expiry, and child failure stay distinct | [Security](../requirements/security.md), [GitHub capabilities](github-app-capabilities.md) |
| Authorize a work session | `engram github grant` | Retain bounded in-memory signing authority for one pane, App, installation, repository set, permission ceiling, purpose, and expiry | `engram github status` reports the active ceiling | `engram github revoke`, expiry, pane change, enrollment change, or restart removes it | [Security](../requirements/security.md), [GitHub capabilities](github-app-capabilities.md) |
| Diagnose locally | `engram inspect` or `engram doctor agent` | Read bounded local state and selected tmux facts without constructing provider or Telegram clients | Control-safe stdout identifies what was and was not proven | The command reports the failing local boundary without changing it | [Operations](../requirements/operations.md), [Headless operation](headless-operation.md) |

## Evidence Rules

Each public operation should make five things recoverable from the repository:

1. the initiating surface;
2. the exact authority or disclosure it requests;
3. the state transition or external effect;
4. the evidence shown on success and failure; and
5. the recovery or revocation path.

A change is incomplete when one of those exists only in prose or only in code.
The implementation, focused tests, binding requirement, and user-facing
documentation should describe the same path with the same terms.

## Distinctions To Preserve

- A user-facing session is not a tmux session. It is the handle for one watch.
- A watch is not its pane. It records and repeatedly validates the pane binding.
- A frame is not a guide or snapshot. Both presentations derive from a frame.
- A current anchor is not every Telegram message that once referred to a watch.
- Local `engram status` is not Telegram `/status` or native `service-status`.
  They inspect different surfaces and should be named explicitly in prose.
- Local `engram preflight` and `engram status` currently run the same read-only
  readiness checks. Their distinction is operational intent: preflight is the
  gate before start or restart; status is the same evidence requested during
  diagnosis. `dry-start` is different because it may initialize local state.
- GitHub approval is not token issuance, access, command success, or repository
  change. Each later step can fail independently.
- A configured capability is not necessarily available. Runtime probes and
  identity checks decide availability at the point of use.

## Review Method

For any user-visible change, walk the affected row from entry through recovery.
Review neighboring rows when the change touches shared identity, state,
presentation, or authority. The pull request template records this check; the
requirements remain the final authority when descriptive documents disagree.
