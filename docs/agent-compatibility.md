# Agent Compatibility

Codex and Claude Code use one provider-neutral compatibility model. Support is
not one boolean: Engram evaluates four independent contracts for every running
agent pane.

| Axis | Codex contract | Claude Code contract | What it proves |
| --- | --- | --- | --- |
| Process | `codex-process-v1` | `claude-process-v1` | One exact foreground process incarnation. |
| Binding | `codex-session-start-v1` | `claude-session-start-v1` | Fresh pane-local hook metadata for that incarnation. |
| Screen | `codex-screen-v1` | `claude-screen-v1` | A tested visible grammar safe for semantic presentation. |
| Transcript | `codex-rollout-v1` | `claude-transcript-v1` | An exact, bounded historical-message parser. |

An unknown screen version does not disable a proven transcript. A missing hook
does not erase visible presentation. Disabled historical context is reported as
disabled, not broken. Process replacement makes process-bound presentation,
viewport, and retained metadata stale immediately.

## Semantic and literal views

Engram preserves the bounded tmux capture as the literal authority for raw
text, screenshots, links, and file references. A second semantic viewport can
exclude an exact startup prelude or tested interface chrome from guide evidence.
The viewport is bound to the provider, grammar contract, process fingerprint,
copy-mode state, and alternate-screen state. Restart, rebind, pane adoption,
process replacement, copy mode, or alternate-screen changes invalidate it.

Ordinary shell output and quoted lookalikes are retained. Startup trimming
requires the complete versioned boundary, not a keyword or a provider name.

## Structured presentation

The session card uses a compact provider line. Its icon-only `ℹ️` control opens
one expandable detail message for that exact session and anchor. Details can
show model and provenance, reasoning effort, interaction mode, activity, last
turn duration, active/total agent counts, and the four source axes. The message
never includes session UUIDs, transcript or executable paths, working
directories, terminal text, task text, or collaborator names. It is edited in
place, dismissed with `✕`, and removed when the session collapses, closes,
rebinds, resumes, or loses its current anchor.

Model provenance is one of:

- `hook`: declared by Claude's documented `SessionStart` model field;
- `visible_ui`: present in the current tested frame; or
- `retained_same_incarnation_ui`: previously visible and retained only for the
  same proven process incarnation.

Engram does not infer a Codex model from undocumented hook fields.

## Read-only doctor

Run the local diagnostic from a watched pane or name one exact pane:

```sh
engram doctor agent
engram doctor agent --provider claude --pane %4
engram doctor agent --env "$HOME/.engram/.env" --provider codex --pane %7
```

The doctor uses the production detectors, grammar analyzers, binding reader,
and transcript parsers. It makes no network calls, writes no Engram state or
pane metadata, sends no keys, and does not change the service. Its output is a
bounded human diagnostic, not a stable machine protocol. It deliberately omits
paths, UUIDs, terminal content, task content, and agent names.

## Adding support

Support declarations require reviewed, sanitized fixtures and explicit tests
for positive recognition, false positives, version mismatch, process change,
and independence from the other axes. Never widen a version merely because its
number is nearby. See [Compatibility fixtures](compatibility-fixtures.md).

