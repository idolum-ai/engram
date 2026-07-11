# Frontend-Independent Core

Status: design proposal; no runtime behavior is committed by this document.

## Decision

Make Telegram optional by separating Engram's tmux behavior from its Telegram
delivery behavior, then running exactly one explicitly selected frontend in each
Engram process.

The intended process shapes are:

```text
engram tui       -> local TUI frontend      -> Engram core -> tmux
engram telegram  -> Telegram frontend       -> Engram core -> tmux
engram mcp       -> experimental stdio only -> Engram core -> tmux
```

`engram run` may remain a compatibility alias for `engram telegram` during a
migration. Frontends are statically linked Go packages selected by subcommand.
They are not discovered plugins and they do not connect to a resident Engram
daemon.

This proposal does not add a Unix socket, HTTP listener, filesystem inbox,
event bus, or simultaneous frontend ownership. The existing exclusive state
lock becomes a cross-frontend guarantee: one Engram home has one mutable core
and one admitted frontend process at a time.

## Why

Telegram currently supplies remote identity, delivery, reply handles, pins,
buttons, attachments, and a phone client. Those are valuable product choices,
but they are also embedded throughout orchestration and state. Merely making
the bot token nullable would leave a Telegram-shaped core full of absent IDs
and conditional behavior.

The useful extraction is smaller and more precise. Engram's local work is:

- bind a local Engram session to immutable tmux pane and window identity;
- validate that identity immediately before capture, input, cwd, or close;
- observe one bounded physical/logical frame;
- keep presentation off the input path;
- preserve current-versus-stale reply generations;
- coalesce bounded refresh and attention work;
- retain local lifecycle, provenance, recovery, and audit state.

Telegram should translate those operations into a private DM experience. It
should not define the operations themselves.

## Invariants That Must Survive

1. tmux remains the workspace and source of terminal truth.
2. One local OS user owns one Engram home. This does not become multi-user.
3. Every effect revalidates `%pane_id` and `@window_id`; names and indexes are
   never authority.
4. Input remains independent of Telegram, Haiku, Chromium, and presentation.
5. One capture supplies physical ANSI and joined logical text over the same
   bounded coordinates.
6. A session has one current canonical view per selected frontend. Replaced
   handles are stale and have no tmux effect.
7. Automatic work remains bounded and slow; explicit refresh remains immediate.
8. Upstream records remain untrusted pane-authored attention, not identity.
9. Created and attached tmux windows retain different close semantics.
10. State, logs, captures, queues, and concurrency remain bounded.
11. Go remains standard-library-only unless a separate design review changes it.
12. Nested operation continues to collapse into the terminal rather than
    creating an Engram hierarchy.

## Process And Authority Model

### One process, one frontend

The selected frontend constructs the core in-process and holds its exclusive
state lock. A TUI and Telegram service cannot race the same state or pane. If a
second frontend starts, it fails with lock metadata naming the current owner.

Exclusivity avoids inventing policy for competing canonical views,
acknowledgements, stale generations, and close confirmations. Supporting
simultaneous frontends would be a separate capability with a separate failure
and authority analysis.

### Admission

- Telegram keeps exact configured user/chat admission and Bot API update
  deduplication.
- TUI and one-shot CLI inherit authority from explicit execution by the owning
  OS account.
- Stdio MCP inherits authority from the process that launches it. It does not
  authenticate or sandbox that parent.

The core receives an already admitted local-owner principal. It does not gain a
user registry, roles, delegated principals, or bearer tokens.

### Current view handles

External delivery identifiers should map to core-owned routes:

```text
route = frontend instance + session ID + view kind + generation
```

Telegram maps chat/message IDs to routes. A TUI maps stable rows or tabs to
routes. An experimental MCP frontend returns an opaque bounded view handle.
Only the current generation of a routable kind may send input; an identity
check still occurs immediately before the effect.

The representation is private. The invariant is public: stale views cannot
act.

## Core Boundary

Extract behavior rather than a generic transport framework. A plausible private
package boundary is:

```text
internal/core/                tmux use cases, identity, lifecycle, frames,
                              generations, scheduling, attention
internal/frontend/telegram/   admission, polling, delivery IDs, pins,
                              rotations, callbacks, Telegram files
internal/frontend/tui/        local terminal interaction and stable views
internal/frontend/mcpstdio/   experimental bounded JSON-RPC translation
internal/tmux/                existing tmux leaf
internal/state/               persistence and migrations
```

The eventual Go API should emerge from characterization tests. It should expose
typed operations such as list, attach, new, refresh, send command, send text,
send keys, rename, watch, and confirmed close. It should return bounded domain
views rather than Telegram markup.

Do not define a broad `Transport` interface mirroring Telegram methods. Pins,
photos, callbacks, documents, and Bot API error classes belong to the Telegram
frontend. Other frontends should not emulate them.

The core must not receive a generic `Execute(string)` callback, raw tmux socket,
arbitrary tmux target, or frontend credential.

## State Direction

Core and Telegram state are currently interleaved. A migration should separate
ownership without deleting unknown state:

- Core-owned: local session ID, immutable pane/window identity, provenance,
  lifecycle, title, cwd, watch state, capture hashes, attention record IDs,
  current route generations.
- Telegram-owned: update offset/journal, configured admission identity,
  message/pin/rotation IDs, Telegram file IDs, attachment bypasses, delivery
  retries.

The final persistence shape is deliberately undecided. A nested state envelope
or a separate Telegram state file may both work. The deciding criteria are
migration safety, atomicity, boundedness, and whether a local-only process can
open core state without inventing Telegram identifiers.

The process lock should derive from canonical `ENGRAM_HOME`, not Telegram
credentials. Telegram may retain a second poller lock keyed by bot/user/chat so
two homes cannot poll the same bot tuple.

## Frontend Assessment

### Local TUI: preferred destination

A foreground TUI can preserve Engram's scan-orient-act rhythm locally: stable
session rows, current selection, bounded literal frame, attention, refresh,
explicit input mode, validated keys, watch, attach/new, and close confirmation.

It must render sanitized structured text. Captured terminal bytes and control
sequences are data and must never be replayed into the controlling terminal.
Literal text mode should require no Telegram token, model key, Chromium, DNS,
or network socket.

### One-shot CLI: narrow companion

Read-only status, sessions, and bounded view commands are useful early probes.
Mutating commands should come later, remain nonresident, take sensitive input
on stdin where practical, and support current-generation preconditions when a
read and write are split across invocations.

### Stdio MCP: experiment, not default

Stdio MCP opens no port, but locality of bytes is not containment of authority.
The MCP host receives the owning user's Engram authority. It may send pane text
to remote models, persist tool results, auto-start tools, or let prompt
injection turn terminal output into shell input.

Any experiment must therefore be a separate explicit `engram mcp --stdio` mode:

- never enabled from credentials or automatic discovery;
- no HTTP, SSE, or URL-selected transport;
- read-only tools first: status, tracked sessions, bounded current view, and
  bounded attention;
- no arbitrary shell, raw tmux target, files, logs, attachments, close, restart,
  full scrollback, or mode changes;
- no unsolicited notifications in the first experiment;
- no automatic Haiku interpretation;
- exit on stdin closure or parent death;
- mutation, if ever tested, requires a new security review, an explicit startup
  grant, typed bounded tools, and current opaque view handles.

Enabling mutating MCP means trusting the host and its model/tool policy as fully
as the local user. The product must say so plainly.

### Rejected first-architecture surfaces

- Unix socket: an ambient same-UID RPC endpoint with lifecycle, peer, framing,
  cleanup, and concurrency costs.
- Loopback HTTP: reachable by browsers, extensions, SSRF, forwarding, and
  container networking; requires web authentication and origin policy.
- Filesystem inbox: durable replayable command authority available while the
  user is absent.
- SSH adapter: SSH already provides transport and authentication; run the TUI
  over an explicit PTY instead.
- Dynamic plugin host: becomes arbitrary code execution and a platform surface.

## Security Claims

The design aims to claim:

- local literal mode makes no network calls and opens no listener;
- exactly one explicit frontend owns mutable state;
- every stale route has zero tmux effect;
- every mutation validates immutable tmux identity;
- frontends receive only capabilities needed for their declared boundary;
- pane content remains untrusted data across local views, Haiku, Telegram, and
  MCP;
- no child process receives credentials, state access, parent tmux access, or a
  command endpoint merely because it runs inside a watched pane.

It does not claim confidentiality from an enabled Telegram, Anthropic, or MCP
boundary, protection from compromise of the owning OS account, simultaneous
frontend consistency, guaranteed attention delivery, or sandboxing of an MCP
host.

## Explicit Non-Goals

- Multi-user or group operation.
- An ambient daemon API or local control plane.
- Network MCP, HTTP, WebSocket, SSE, webhook, or discovery.
- Simultaneous mutable frontends.
- Filesystem command queues or guaranteed delivery.
- Dynamic adapters, plugins, or a marketplace.
- Generic shell execution or arbitrary tmux proxying.
- Autonomous conversion of pane text into actions.
- Cross-session model memory or a general agent runtime.
- Reproducing Telegram attachments, pins, and media behavior in every frontend.
- Replacing tmux or streaming unbounded pane output.

## Open Decisions

1. Whether the local destination should begin as a read-only CLI before a TUI.
2. Whether route generations belong in existing session records or a separate
   bounded structure.
3. Whether Telegram delivery state remains in `state.json` or moves to a
   separately atomic file.
4. Whether `engram run` remains a permanent alias or receives a deprecation path.
5. Whether local read-only queries require the exclusive lock or can use a
   proven snapshot-only path.
6. Whether a read-only stdio MCP experiment earns its maintenance cost after
   the local TUI exists.

These questions should be answered by the staged plan, not by adding general
abstractions in advance.
