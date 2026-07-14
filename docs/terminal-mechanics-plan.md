# Terminal Mechanics Extraction Plan

Status: implemented sequence and continuing review gate.

This plan records how the narrow boundary described in
[`terminal-mechanics-boundary.md`](terminal-mechanics-boundary.md). It preserves
Telegram as Engram's only product surface and
[`protocol-posture.md`](protocol-posture.md) as the limit on protocol work.

## Stage 0: Characterize Before Extracting (complete)

Goal: identify the existing tmux-shaped rules without changing ownership or
runtime behavior.

Work:

- Add focused black-box coverage for immutable pane identity, conclusive pane
  loss, created-versus-attached close, shared-frame coordinates, typed input,
  and terminal attention records.
- Inventory `internal/app` and persisted state by actual owner: terminal
  mechanics, Telegram delivery, presentation, or orchestration.
- Record Telegram behavior around anchors, stale replies, callbacks, refresh,
  attachments, and restart recovery as compatibility fixtures.
- Identify the smallest call graph that can move without taking scheduling or
  Telegram concepts with it.

Exit criteria:

- Each proposed mechanics invariant has focused coverage.
- Telegram types and message identifiers crossing the candidate boundary are
  enumerated.
- No runtime, state, configuration, or command behavior changes.

Stop if the candidate boundary is primarily an interface over `internal/app`
rather than ownership of tmux truth.

## Stage 1: Extract One Vertical Slice (complete)

Goal: prove the boundary with immutable pane identity and one typed effect.

Work:

- Move pane/window binding and effect-time validation behind a private package.
- Move one typed input operation through that validation path.
- Keep admission, reply resolution, stale-message rejection, auditing, and
  acknowledgement in Telegram orchestration.
- Add import checks preventing the mechanics package from importing Telegram,
  presentation, or delivery identifiers.

Exit criteria:

- Existing Telegram behavior is unchanged.
- The mechanics tests construct no Telegram or renderer client.
- A stale Telegram reply still has zero tmux effect.
- Input latency and worker bounds do not regress.
- The extracted API contains no generic execute function or arbitrary target.

Rollback the slice if it adds more translation than clarity.

## Stage 2: Complete Only The Earned Boundary (complete)

Goal: move the remaining mechanics that share the same truth and failure model.

Candidate work:

- created-versus-attached lifecycle and confirmed close effect;
- bounded physical/logical frame capture;
- literal text and validated key input;
- cwd and rename tmux operations;
- terminal attention-record parsing and deduplication.

Move each candidate independently. Scheduling, anchor generations, retries,
attachments, rendering, and service lifecycle stay in application or Telegram
ownership unless a concrete duplication proves otherwise.

Exit criteria for every move:

- tmux is the sole source of the extracted result;
- failures remain typed and conservative;
- state migration is unnecessary or separately reviewed;
- existing integration and real-tmux tests remain green;
- total conceptual surface does not grow.

There is no requirement to move every candidate.

## Stage 3: Add A Read-Only Architecture Probe (complete)

Goal: demonstrate that terminal mechanics can be observed without constructing
Telegram, not to create another control surface.

Maximum commands:

```text
engram inspect status
engram inspect sessions
engram inspect frame <watch-id>
```

Constraints:

- no mutation, stdin command input, arbitrary tmux target, full scrollback, file
  access, attachment access, logs, renderer, or model;
- no Telegram, model-provider, or Chromium configuration;
- no network calls, DNS, listener, resident process, or background worker;
- bounded sanitized text output only;
- no new persisted state and no fabricated delivery identity;
- conservative handling of locked, corrupt, or future-version state.

The command should primarily serve diagnostics, tests, and maintainers. It must
not be presented as a Telegram replacement.

Exit criteria:

- an executable no-egress test proves no network client is constructed;
- output is bounded and hostile terminal controls remain inert;
- normal Telegram service operation and locking remain unaffected;
- deleting the probe would not collapse the mechanics boundary.

Stop if read-only inspection requires generic routes or frontend abstractions.

## Stage 4: Stop And Reassess (complete)

After the extraction and probe, measure:

- whether pane identity and failure tests became simpler and stronger;
- whether Telegram orchestration became easier to read;
- whether input remains immediate;
- whether package and state vocabulary shrank;
- whether the boundary has one clear reason to change.

Delete or fold back abstractions that did not earn their maintenance cost.
Do not proceed automatically into a TUI, local mutation API, MCP server, daemon,
or generic carrier interface.

A future carrier proposal is a fresh product decision. It must show a real
phone-first, low-dwell workflow before changing Engram's Telegram requirement or
extracting shared delivery concepts.

The reassessment kept the private mechanics boundary because it now enforces
identity immediately around pane effects and has two real callers: Telegram
orchestration and local inspection. It kept scheduling, lifecycle state,
provenance, attention parsing, and every anchor concept with their existing
owners. No generic interface, persistent state, background process, credential,
or network boundary was added. The inspector remained read-only and the tmux
leaf gained only one bounded literal capture operation.

## Cross-Stage Gates

Every implementation PR must answer:

- Does input remain independent of presentation and delivery?
- Does each effect revalidate immutable pane/window identity?
- Does Telegram retain sole ownership of anchors and stale reply semantics?
- Does pane content remain data rather than authority?
- Is state and work still bounded?
- Is there still no listener, inbox, plugin discovery, or child endpoint?
- Does the package use only Go's standard library?
- Could deleting the new abstraction make Engram clearer?

## Review Sequence

1. Characterization tests and ownership inventory.
2. Pane identity plus one typed input slice.
3. Independently reviewed lifecycle, frame, input, and attention moves that
   satisfy the boundary tests.
4. Read-only no-egress architecture probe.
5. Reassessment and deletion pass.

These slices may be reviewed as commits in one implementation PR when their
combined diff remains coherent. They must remain independently understandable
and reversible. Do not combine state migration with package extraction or a new
user-facing surface; this implementation requires no state migration.
