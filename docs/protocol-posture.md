# Protocol Posture

Status: design proposal; this document defines direction, not a new wire API.

## Position

Engram should become more protocol-like in its documented invariants and in the
terminal-native attention convention. It should not become a general network
protocol, event platform, or adapter ecosystem.

Protocol discipline is useful when it lowers the user's cost of trusting a
brief glance and a remote action. It is harmful when it creates another service,
credential, endpoint, compatibility matrix, or inbox to operate.

The boundary to standardize is truth and attention, not transport.

## What Engram Is

tmux is already the durable workspace. Engram contributes:

1. a cheap handle for addressing a watched pane;
2. one bounded observation of that pane;
3. a stable current view that can route a reply;
4. conservative recovery when capture, presentation, delivery, or persistence
   fails partway through an operation.

The product measure is time to trustworthy orientation plus time to safe next
input across many panes. Protocol work is justified only when it improves that
measure or protects it from ambiguity.

## Irreducible Nouns

- Pane identity: the immutable `%pane_id` and `@window_id` pair validated at
  effect time.
- Watch: Engram's local record binding a user-facing session ID to pane identity,
  provenance, lifecycle, and observation state. It is not a tmux session.
- Frame: one bounded physical ANSI and joined logical observation over shared
  coordinates.
- Current view: the one canonical presentation for a watch in the selected
  frontend.
- Route: a current view or latest alternate that may request input for the same
  watch. Superseded routes are stale.
- Input action: command plus Enter, literal text without Enter, or validated keys.
- Attention record: a bounded terminal-authored request to look, with a random
  deduplication ID but no sender identity.

These nouns are enough to state Engram's important transitions without exposing
Telegram IDs, state-file fields, scheduler maps, or renderer mechanics.

## Invariants Worth Standardizing

1. Pane-bound effects require the stored immutable identity pair to validate.
   Timeout or generic failure does not prove loss.
2. Input kinds remain distinct and presentation never blocks their critical path.
3. Physical and logical presentations derive from one bounded frame.
4. A watch has at most one actionable current view per selected frontend.
5. Only the latest route of each alternate kind may act; known stale routes fail
   closed and explain why.
6. Replacement records the successor before retiring the predecessor wherever
   the external medium permits.
7. Attention records are best effort, bounded, deduplicated, terminal-authored,
   and untrusted. They never become commands or authentication.
8. Uncertain shell effects are not replayed after restart.
9. Presentation failure does not kill tmux work or falsely claim pane loss.

Requirements and black-box tests are the compatibility surface for these
promises. They do not require a public Go interface or serialized event stream.

## Protocol Levels

### Documented semantics: adopt

Name the invariants, preconditions, transitions, and failure outcomes. Keep
requirements binding and executable. This is largely a clarification of what
Engram already does.

### Terminal attention record: adopt deliberately

The existing BEL plus `[engram:upstream]` record is Engram's natural open
producer/consumer boundary. Independent programs can emit it through the PTY
without credentials, discovery, or topology.

Keep it one-way, text-only, bounded, best effort, and visibly untrusted. Do not
add acknowledgement, sender identity, attachments, typed jobs, guaranteed
delivery, or direct routing to nested sessions under the same version.

### Internal event vocabulary: use sparingly

Names such as frame observed, pane identity lost, input applied, view
superseded, and attention requested can sharpen tests and audit records.

Do not turn that vocabulary into event sourcing, replay, projections, or a
public stream. Replay is dangerous for shell input, and eventual consistency
weakens current-view ownership.

### Private capability interfaces: only when earned

Narrow interfaces are useful when they enable fault injection or separate a
real owner, such as the proposed private terminal-mechanics boundary. They
should remain private and typed.

Do not publish a terminal-provider, chat-provider, or plugin SDK. tmux and
Telegram are productive constraints, not placeholders for arbitrary backends.

### General wire protocol: reject

A socket, HTTP API, RPC stream, or cross-host peer would duplicate tmux locally
and Telegram remotely while adding credentials, discovery, version negotiation,
backpressure, and another failure domain. It does not directly reduce phone
attention.

If a future requirement needs guaranteed background delivery, it is a new
capability and security model, not a silent extension of the attention record.

## What Remains Private

- State-file layout, field names, pruning arrays, hashes, and atomic-write helpers.
- Telegram update/message IDs, Bot API calls, pin reconciliation, and media
  migration mechanics.
- Scheduler cadence, queues, locks, semaphores, and worker counts.
- Chromium invocation, image dimensions, and temporary files.
- Model-provider request shape, prompt wording, model ID, and prose formatting beyond
  the declared bounded-frame privacy boundary.
- Audit JSON shape unless an operator-facing compatibility promise is made.
- Local ID allocation and title heuristics.

Publicly versioning these would constrain one implementation without enabling
useful composition.

## Smallest Decisive Experiment

Treat the terminal attention record as the only candidate open protocol.

1. Write a one-page normative attention-record v1 specification without
   changing current bytes.
2. Add conformance fixtures for valid records, malformed IDs, control
   characters, oversized UTF-8, duplicates, wrapping, and newest-record choice.
3. Add one independent POSIX-shell emitter that neither invokes Engram nor reads
   its state or credentials.
4. Exercise it through two real PTY boundaries, such as SSH `-t` and container
   exec `-t`, during ordinary multi-pane work.
5. Compare missed, duplicate, stale, accidental, and useful signals against
   `engram signal` without adding retries or acknowledgements.

Protocolization succeeds if a second producer is trivial, the outer pane remains
the truthful route, and pane-checking time falls without a new service or mental
model. It fails if usefulness immediately demands durable queues, identity,
acknowledgements, typed jobs, or inner-session routing.

## Decision Rule

Engram should not become a protocol in order to become larger. It should use
protocol discipline to stay small: precise about what a reply means, modest
about what a frame knows, honest about what an attention record proves, and
quiet about everything else.
