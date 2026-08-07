# Claude Code Session Context

Engram can bind an exact Claude Code session to an exact watched tmux pane for
automatic recovery. A separate, disabled-by-default privacy setting can also
admit bounded visible conversation text as historical guide context.

## Enable the lifecycle hook

Install Engram first, then merge this hook into `~/.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume|clear|compact|fork",
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.local/bin/engram claude-hook",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

Use Claude Code's `/hooks` browser to verify the event, matcher, command, and
settings source. The command receives official `SessionStart` JSON on stdin,
including `session_id`, `transcript_path`, `cwd`, and `source`. It inherits
`TMUX_PANE`, prints no context, and writes only bounded provider-session
metadata to that pane.

Claude's official `SessionStart` payload also includes `model`. Engram accepts
that bounded token as declared presentation metadata with `hook` provenance;
it does not use the model to locate a process or transcript. A visible model
card takes precedence, and the declaration is discarded on process replacement,
rebind, resume preparation, or pane adoption.

The service treats hook output as a candidate, not proof. It validates the
watched tmux server, window, and pane around every metadata read. Automatic
recovery retains only the allowlisted provider and UUID and reconstructs:

```sh
claude --resume <session-uuid>
```

## Migrate an already-running session

From inside the exact existing Claude session, run:

```sh
engram claude-bind
```

The command accepts no UUID or pane argument and does not print the UUID. It
uses the inherited `TMUX_PANE`, proves exactly one active descendant Claude
process, validates its executable version and precise process incarnation,
then checks the bounded PID registry and exactly one UUID-named transcript.

Claude's PID registry is not a documented public contract. This command is a
fail-closed migration convenience only. If any field, file, process, or schema
cannot be proven, restart Claude after installing the official hook.

## Enable historical context

Recovery binding does not read message text. Historical context requires a
separate explicit setting in Engram's environment file:

```text
ENGRAM_CLAUDE_CONTEXT_TURNS=4
```

Accepted values are `0` through `8`; `0` is the default and disables transcript
reads. Restart Engram after changing the setting. `engram status` reports the
configured bound without displaying UUIDs, paths, or message text.

## Admission and identity contract

Engram reads only the exact absolute transcript path supplied by the bound
Claude lifecycle hook. It requires:

- a valid UUID matching the transcript filename;
- an owned regular non-symlink file;
- matching `sessionId` values in the bounded transcript records;
- one proven Claude process descended from the tmux pane;
- a stable PID, absolute executable, version, and precise process-start
  identity;
- a hook observation no older than that process;
- the same process, provider binding, transcript locator, and watched tmux
  identity after the read; and
- the same identities again at final guide publication.

There is no newest-file, working-directory, title, model, or timestamp fallback
for session identity or transcript selection.
Missing transcript persistence is an ordinary unavailable-context outcome; tmux
monitoring continues normally.

Large transcripts are never read in full without a bound. Engram verifies the
session identity in a bounded prefix and reads recent complete JSONL records
from a fixed tail ending at the file size observed when it was opened. File,
line, record, message, and aggregate prompt sizes have independent limits.

## `claude-transcript-v1`

Anthropic documents the JSONL entry format as internal and subject to change.
Engram's parser is therefore narrow and versioned. It admits only:

- non-meta, non-sidechain human `user` records whose message content is a
  string; and
- main-session `assistant` content blocks explicitly typed `text`.

It excludes:

- thinking and redacted thinking;
- tool calls, arguments, and results;
- array-form user records;
- system and generated metadata;
- attachments and non-text blocks;
- sidechains and subagent transcripts; and
- unknown content inside an otherwise recognized candidate message.

Split assistant records are combined only when they carry a stable message
identity, and repeated text blocks are deduplicated. Unknown unrelated
top-level records are ignored. A changed recognized message shape fails closed
to terminal-only guidance until a sanitized fixture establishes support.

The configured Engram redactor runs on complete decoded messages before
per-message truncation and again before provider delivery. Transcript text is
never placed in state or audit output.

## Guide and diagram behavior

Admitted messages are labeled `historical_session_context` in the guide prompt.
They may clarify prior topic or intent but cannot establish current effects,
completion, files, links, hashes, or terminal state. Current tmux capture
remains the only current-state authority.

The historical fingerprint participates in guide continuity, so a new visible
message can refresh an otherwise unchanged pane. A clear, resume, fork,
process replacement, provider rebind, pane replacement, or lost transcript
invalidates the old context before it can publish.

A deterministic local detector may copy one safe box/arrow diagram from the
admitted messages. It does not ask a model to select or repair the text. The
inset is labeled either:

```text
Claude context · prior visible message, not current terminal
Claude context · reconstructed from visible terminal text
```

The stronger label requires one exact unique match in current semantic tmux
text. Redaction conflicts, unsafe geometry, ordinary prose, code-like blocks,
and weak candidates are omitted. Literal snapshots and raw terminal views never
receive transcript-derived content.

## Diagnostics

Bounded `terminal.claude_context` audit records report only the watch ID,
provider, outcome, reason, message count, and diagram presence. They do not
contain the UUID, transcript path, working directory, executable path, terminal
text, or transcript text. Repeated identical outcomes are coalesced.

Useful reason classes include unproven process identity, unproven session
identity, unavailable transcript, changed identity, no visible messages, and
redaction conflict. Eligible outcomes also record the bounded Claude version
and active parser name as separate fields.

## Operational and release checklist

For a new installation or controlled rollout:

1. Keep `ENGRAM_CLAUDE_CONTEXT_TURNS=0` while installing the hook.
2. Confirm the `SessionStart` hook and its settings source in Claude Code's
   `/hooks` browser.
3. Start a disposable Claude session inside a watched tmux pane and confirm
   recovery metadata is accepted without UUIDs or paths appearing in logs.
4. Check `engram status` for the configured Claude context bound and inspect
   `terminal.presentation` plus `terminal.claude_context` reason codes.
5. Exercise startup, resume, clear, compact, fork, model switch, Engram restart,
   Claude process replacement, and recovery of a disposable tmux session.
6. Inspect bounded audit output for the absence of terminal and transcript
   text, UUIDs, working directories, and transcript or executable paths.
7. Enable a small nonzero context bound for one controlled watched session,
   then set it back to zero and confirm the next refresh is terminal-only.

The dedicated UI adapter and transcript context are version-gated separately.
Every newly encountered Claude Code release remains unsupported until a change
adds sanitized screen and transcript fixtures that cover executable identity,
foreground naming, hook fields and lifecycle sources, visible message shapes,
sidechains and tools, model switching, activity and approval UI, and resume
behavior. Unknown versions retain literal terminal behavior and omit historical
context; maintainers must not widen a version range based only on a version
string or one successful session.

## Disclosure

When context is enabled, admitted redacted text is sent to the selected Engram
guide provider under that provider's normal retention policy. An admitted
diagram may also be delivered to Telegram. Engram does not persist either form
as transcript context, but the external services receive what Engram sends.

Official Claude Code references:

- <https://code.claude.com/docs/en/hooks>
- <https://code.claude.com/docs/en/sessions>
