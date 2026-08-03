# Agent Screen Semantics

Engram normally interprets agent terminal interfaces by visible structure
rather than by an executable name or CLI version. Two narrow compatibility
adapters supplement that rule for exact, tested Codex and Claude Code versions.
The goal is deliberately narrow: keep conversation evidence legible for the
guide and expose model and activity state on the card without making the
terminal less truthful.

The raw `CaptureStyled` frame remains the source of truth. It is still used for
screenshots, inspection, links, and file references. Semantic interpretation
produces a second, derived text presentation; it never edits the pane or feeds
text back into it.

## Semantic roles

The bounded analyzer can label visible rows as:

- user message;
- assistant message;
- tool invocation or result;
- approval question;
- active work or approval review;
- passive composer;
- model/status metadata; or
- terminal-interface chrome.

Each labeled region retains its zero-based source row range, confidence, and
the structural evidence used to classify it. Unknown text stays unknown and is
preserved.

The normalized activity vocabulary is `unknown`, `idle`, `active`, and
`awaiting_approval`. It describes what the screen shows, not what Engram thinks
the underlying process is doing.

## Structural anchors

Interpretation requires a strong low-band anchor and a known model identifier.
The model may be in a conventional `model effort · path` footer, embedded in a
low-band `▣ label · model [effort] [fast]` row, or displayed in a stable model
card while effort and composer state appear below. Provider-qualified models
require an allowlisted provider identifier and a valid complete token. The
registry recognizes model families; it does not recognize product versions.

Engram then combines independent visible signals:

- position near the bottom of the frame;
- prompt, tree, separator, spinner, and status glyphs;
- exact duration and interrupt affordances;
- stable model/status text;
- adjacency between composer and status regions; and
- a single previous frame when the pane, foreground command, dimensions,
  alternate-screen state, and copy-mode state remain compatible.

Temporal evidence is process-local, limited to one previous frame per tracked
session, and never persisted. Reattachment or identity change discards it.

Claude Code's model card may scroll away while its composer and activity rows
remain visible. For explicitly supported Claude Code versions, Engram confirms
the executable through the pane's process tree and derives a process-incarnation
fingerprint from its absolute executable, version, PID, and start time. A model
visibly proven earlier for that same fingerprint may then remain the structural
model anchor. The fingerprint and model survive an Engram service restart
because systemd does not own the tmux child process. A changed process
fingerprint discards the model. A later visible model card replaces it, which
covers in-session model switches.

Linux resolves the running executable through `/proc/<pid>/exe`. Engram does
not substitute a `$PATH` lookup on platforms that cannot expose that identity;
the dedicated Claude adapter therefore fails closed on current macOS builds
while the generic structural analyzer remains available.

Runtime confirmation may also expose Claude's activity without a known model.
In that case the card says `Claude` with effort and activity, but no model is
guessed. The structural anchor may be the effort/status row or the observed
pair of separators enclosing Claude's composer. Exact completed elapsed rows,
the low-band composer/status controls, and Claude's `/clear` token-saving hint
are omitted from guide evidence only inside this versioned boundary.

## Exact Codex session context

`ENGRAM_CODEX_CONTEXT_TURNS` is a separate, disabled-by-default privacy surface.
When enabled, it admits up to eight recent user turns and their visible assistant messages as
historical guide context only after all of these independent checks succeed:

- the watched tmux server, window, and pane binding validates around the
  pane-local recovery option;
- a `SessionStart` hook provides one syntactically valid Codex UUID;
- the active pane process tree contains one proven Codex executable and yields
  a PID/path/version/start-time incarnation fingerprint;
- the hook observation is not older than that process incarnation;
- exactly one regular, non-symlink rollout filename carries the UUID, and its
  `session_meta.id` repeats it in a bounded prefix read; recent records come
  from either the same bounded full-file read or a bounded tail ending at the
  file size observed when opened; and
- the same process incarnation and tracked tmux binding still exist after the
  rollout read.

There is no newest-session, working-directory, title, or model-based fallback.
The parser contract is explicitly named `codex-rollout-v1`. It ignores unknown
record types and admits text only from `response_item` records whose payload is
a `message`, whose role is `user` or `assistant`, and whose content type matches
`input_text` or `output_text`. System/developer roles, hidden reasoning, tool
arguments/results, attachments, and generated environment/instruction metadata
are excluded. Unrecognized structure in a recognized message fails closed.
Messages, individual text, rollout read windows, JSON lines, and aggregate
prompt text all have independent bounds. The ordinary Engram redactor runs
before provider delivery, and transcript text is never added to state or audit
output.

The guide prompt labels this field `historical_session_context`. It may clarify
past topic and intent, but `terminal_text` remains the only current-state truth.
The transcript fingerprint participates in guide capture and continuity hashes,
so new context can refresh an otherwise stable pane and replacement or loss
rebases continuity.

One deterministic diagram detector examines only those admitted visible
messages. It requires a bounded multi-row box or arrow structure, measures
Unicode terminal-cell width, rejects controls, tabs, oversized candidates,
ordinary prose, source-code-shaped blocks, and weak single arrows, and selects
the latest qualifying block without a model. A redaction conflict removes the
diagram rather than drawing placeholders into it. Guide-evidence images render
the copied text in a distinct `Codex context` inset. Exact unique terminal
mapping is labeled reconstructed; otherwise the label explicitly says the text
is a prior message and not the current terminal. Literal snapshot and raw paths
do not accept this field.

## Fail-closed behavior

Frames longer than 64 rows, unknown model identities, weak shell-like
collisions, unrecognized footer fields, and chrome-only results pass through
byte-for-byte. An ordinary shell line that merely contains a model-like word is
not enough.

Only high-confidence interface rows in proven spatial regions are omitted from
the derived conversation: model/status rows, separators, completed elapsed
decorations, active spinners, an exact completed-approval notice, and passive
composers. Actual user prompts, approval questions, assistant messages, command
invocations, results, keyboard guidance, and unknown approval prose remain
evidence.

The process-confirmed Codex adapter remains a fallback for supported Codex
versions when a frame is too weak for the generic structural contract. The
Claude adapter runs first because its remembered model is part of the
structural proof. Engram currently supports Claude Code `2.1.219` plus the
hermetic `2.1.206` fixture version. Unsupported versions, ambiguous process
trees, unreadable process identity, and unknown layouts preserve the captured
text byte-for-byte. Detection failures do not erase the last card state.

Bounded `terminal.presentation` audit records report program, version, outcome,
reason, whether a model was present, and normalized activity. Identical
decisions are coalesced per session. Raw terminal text and executable paths are
not recorded.

## Tests

The ordinary test suite replays a checked-in corpus covering observed Codex,
sanitized Claude Code `2.1.219`, hermetic Claude Code `2.1.206`, and OpenCode
structures plus false-positive, model-switch, process-replacement, and
identity-change cases. These tests are deterministic, stdlib-only, and make no
network calls.

The optional `agent-ui` E2E suite starts the real Codex, Claude Code, and
OpenCode binaries in separate private tmux servers and private home/config/cache
trees. Test-only drivers point each client at a loopback streaming model server.
A loopback proxy rejects and records optional external requests, and the
harness supplies only obvious fake credentials. No user credentials or config
are read. This is credential and application-state isolation, not OS-level
egress enforcement: a client could bypass proxy variables with a direct socket.
The suite retains active and idle text, semantic JSON, versions, request paths,
and a rendered idle PNG for review.

The drivers live only in the E2E test package. They are not a production plugin
interface and do not expand Engram's authority over terminal processes.
