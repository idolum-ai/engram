# Agent Screen Semantics

Engram interprets agent terminal interfaces by visible structure rather than by
the executable's name or a CLI version. The goal is deliberately narrow: keep
conversation evidence legible for the guide and expose model and activity state
on the card without making the terminal less truthful.

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

The older process-confirmed Codex adapter remains a fallback for supported
Codex versions when a frame is too weak for the generic structural contract.
This keeps already-proven layouts working while the generic analyzer remains
independent of a particular agent CLI.

## Tests

The ordinary test suite replays a checked-in corpus covering observed Codex,
Claude Code, and OpenCode structures plus false-positive and identity-change
cases. These tests are deterministic, stdlib-only, and make no network calls.

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
