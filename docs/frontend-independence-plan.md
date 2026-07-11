# Frontend Independence Plan

Status: proposed sequence; implementation requires later reviewed PRs.

This plan implements the direction in
[`frontend-independent-core.md`](frontend-independent-core.md) while preserving
the protocol posture in [`protocol-posture.md`](protocol-posture.md). Stages are
ordered experiments with stop conditions, not a commitment to ship every
frontend.

## Stage 0: Characterize Current Invariants

Goal: make extraction safer without changing package ownership or behavior.

Work:

- Add black-box tests for pane identity, created-versus-attached close,
  current/stale aliases, shared-frame capture, input independence, upstream
  attention, and crash/retry boundaries.
- Inventory every `internal/app` field and state field as core-owned,
  Telegram-owned, presentation-owned, or temporary orchestration.
- Record current Telegram behavior as compatibility fixtures.
- Draft the exact design-principle and requirement changes that would be needed
  when Telegram stops being mandatory. Do not change the product claim yet.

Exit criteria:

- Every invariant listed in the architecture document has focused coverage.
- Telegram types crossing proposed core operations are enumerated.
- No runtime behavior changes.

Stop if characterization reveals that current anchors cannot be separated from
Telegram without weakening latest-only routing.

## Stage 1: Extract Core Operations Under Telegram

Goal: prove the boundary while Telegram remains the only frontend.

Work:

- Introduce a private core around tmux identity, watch lifecycle, bounded frame,
  typed input, route generations, close provenance, scheduling, and attention.
- Keep polling, admission, message delivery, pins, callbacks, attachments, and
  Telegram update journaling in the existing frontend path.
- Replace Telegram-shaped core inputs and outputs with typed operations and
  bounded views.
- Enforce import boundaries so core cannot import Telegram, Bot API markup, or
  frontend delivery identifiers.

Exit criteria:

- Existing Telegram tests and live behavior remain unchanged.
- Core tests construct no Telegram client and contain no Telegram types.
- Input latency and worker bounds do not regress.
- A stale route still has zero tmux effect after extraction.

Rollback: delete the boundary rather than keep an abstraction that only renames
Telegram methods.

## Stage 2: Add A No-Network Local Read Probe

Goal: prove that the extracted core can orient a local user without Telegram.

Work:

- Add explicit read-only commands for status, tracked sessions, and one bounded
  literal view.
- Select state by `ENGRAM_HOME`; do not synthesize chat IDs or require Telegram
  configuration.
- Define whether read-only commands take the exclusive lock or use a proven
  atomic snapshot path.
- Add an executable no-egress test proving no Telegram, Anthropic, Chromium,
  DNS, or listener is constructed.

Exit criteria:

- Useful orientation works with tmux and local state only.
- The command opens no socket and makes no network call.
- Corrupt or future state fails with the same conservative semantics as today.

Stop if local mode requires frontend-specific placeholders in core state.

## Stage 3: Build The Foreground Local TUI

Goal: make Engram complete and useful without Telegram.

Minimum surface:

- stable session list and selection;
- one sanitized bounded literal view;
- attention marker and explicit refresh;
- attach, new, watch, rename, and current working directory;
- distinct command, text, and validated-key input modes;
- explicit close confirmation preserving provenance.

Constraints:

- Standard library only; use conservative terminal capabilities rather than a
  dependency-heavy widget framework.
- Never replay captured ANSI or control bytes into the controlling terminal.
- Stable actions carry current route generations.
- Presentation remains off the input path.
- No network calls, background daemon, or simultaneous Telegram process.

Exit criteria:

- A user can scan, orient, act, and move among many panes locally.
- Resize and hostile-control fixtures remain legible and inert.
- Telegram credentials can be absent from the local configuration.

Stop if the TUI grows into a terminal emulator or consumes more attention than
tmux itself.

## Stage 4: Make Telegram An Explicit Optional Frontend

Goal: preserve the current remote product while removing Telegram from core
startup requirements.

Work:

- Add `engram telegram`; keep `engram run` as a compatibility alias until a
  separately documented deprecation decision.
- Validate only the selected frontend's configuration.
- Change the Linux service unit to execute the Telegram subcommand explicitly.
- Migrate state losslessly so Telegram delivery data is optional and core state
  does not invent chat identity.
- Key the core writer lock by canonical Engram home; retain a Telegram poller
  lock for the bot/user/chat tuple.
- Make diagnostics and privacy output frontend-specific.

Exit criteria:

- Existing Telegram users see no anchor, reply, attachment, input, or recovery
  regression.
- Local mode starts with no Telegram values and makes zero Telegram calls.
- Starting a second mutable frontend fails clearly before any effect.

Rollback: preserve the compatibility alias and state reader until every prior
release can migrate forward without data loss.

## Stage 5: Consider Narrow Local Mutation

Goal: determine whether scripts need more than the TUI.

Only if demanded, add explicit nonresident commands for new, attach, refresh,
command, text, and validated keys. Sensitive input should use stdin. Commands
that act on a previously observed session should accept a generation
precondition.

Do not add file exfiltration, logs, attachments, full scrollback, background
watching, a generic execute command, or arbitrary tmux targets.

Exit criterion: scripts remain deterministic, exclusive, bounded, and stale
writes fail. Otherwise stop at read-only CLI plus TUI.

## Stage 6: Consider Read-Only Stdio MCP

Goal: test usefulness before granting an LLM host terminal authority.

Surface:

- stdio JSON-RPC only;
- status, tracked sessions, current bounded literal view, bounded attention;
- opaque current view handles;
- no mutations, files, logs, attachments, full scrollback, Haiku, listener, or
  unsolicited notification.

Tests:

- EOF and parent-death shutdown;
- invalid and oversized JSON;
- bounded output, cancellation, and backpressure;
- no network calls;
- host retention and remote-model data-flow review for each supported host.

Exit criterion: read-only MCP materially reduces orientation time without
becoming ambient or causing users to misunderstand its trust boundary.

If read-only MCP is not useful, stop. Do not add mutation to manufacture value.

## Stage 7: Mutation Requires A New Decision

Mutating MCP is outside this plan. Any proposal must receive a new security and
product review and must, at minimum:

- require an explicit startup grant that changes tool discovery;
- trust and name the entire host/model/tool-policy boundary;
- require current opaque view handles and typed bounded input;
- demonstrate host-side human confirmation where available;
- prove pane-authored prompt injection cannot mutate without the configured
  grant and confirmation;
- omit generic shell, arbitrary tmux, files, logs, close, and restart.

The strongest safe outcome may remain no mutating MCP.

## Cross-Stage Gates

Every implementation stage must answer:

- Can stale handles be shown to have zero tmux effect?
- Does each mutation revalidate immutable pane/window identity?
- Can the selected frontend run without constructing unselected network clients?
- Does core compile and test without frontend types or credentials?
- Does pane content remain data rather than authority?
- Is there still no listener, command inbox, plugin discovery, or child endpoint?
- Does the change lower orientation/action time across many panes?
- Could deleting the abstraction make the system clearer? If yes, delete it.

## Suggested PR Sequence

1. Characterization tests and ownership inventory.
2. Private core extraction with unchanged Telegram behavior.
3. Read-only local probe and no-egress contract.
4. Foreground local TUI.
5. Explicit Telegram frontend and state/config migration.
6. Optional narrow CLI mutation, only with evidence.
7. Read-only stdio MCP experiment, only after the local product is whole.

Each PR should be independently reversible and should not combine state
migration, frontend extraction, and a new interaction surface in one diff.
