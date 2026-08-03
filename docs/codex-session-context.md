# Codex session context

Engram can add a bounded number of recent visible Codex messages to guide
requests when `ENGRAM_CODEX_CONTEXT_TURNS` is set to `1` through `8`. This is a
privacy opt-in. Literal snapshots and raw captures remain terminal-only.

Engram never selects a Codex rollout because it is newest or shares a working
directory. It requires an exact session UUID bound to the watched tmux pane,
then independently verifies the active Codex process, exact rollout filename,
matching `session_meta`, and unchanged pane/process identity around the read.

## Existing active sessions

An active Codex session that started before the lifecycle hook was installed
can publish a one-time binding without restarting or clearing its conversation.
Run this command from inside that exact Codex session:

```sh
engram codex-bind
```

The command accepts no UUID or pane arguments. It reads the exact
`CODEX_THREAD_ID` supplied to commands by Codex and the inherited `TMUX_PANE`,
then publishes bounded metadata to that pane. It does not print the UUID, read
the rollout, inspect other sessions, or claim that the binding was accepted.
The running Engram service performs the same process, rollout, and immutable
tmux checks before using it.

Each pre-existing active session must run the command once. Watched panes
advertise this discoverably through the `@engram_codex` tmux option:

```sh
tmux show-options -pv @engram_codex
```

## Future sessions

Install Engram's narrow Codex `SessionStart` hook in `~/.codex/hooks.json`:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume|clear|compact",
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.local/bin/engram codex-hook",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

Review and trust the hook with Codex's `/hooks` interface. Engram does not edit
Codex configuration during installation.

## Verification

After a guide refresh, inspect only the bounded decision records:

```sh
tail -n 100 "$HOME/.engram/audit.jsonl" |
  jq -c 'select(.type == "terminal.codex_context") |
    {at, status, session_id: .data.session_id, reason: .data.reason,
     messages: .data.messages, diagram: .data.diagram}'
```

`status: "applied"` means the exact binding and rollout checks succeeded.
`session_identity_unproven` means the pane lacks a usable binding or the active
process cannot be proven. `rollout_unavailable` means the exact file was absent,
ambiguous, malformed, or outside another bounded parser contract. Engram falls
back to terminal-only guidance in every unavailable case.

Long-lived rollout files may exceed Engram's fixed read budget. Engram verifies
their identity from a bounded prefix and reads recent messages from a bounded
tail ending at the size observed when the file was opened; total work remains
bounded.

## Disclosure boundary

Only visible `user` and `assistant` message text is eligible. System and
developer instructions, hidden reasoning, tool calls and results, attachments,
and generated environment metadata are excluded. Text is bounded and redacted
before it is sent to the configured guide provider. An admitted ASCII or
Unicode diagram may also be sent to Telegram as a separately labelled guide
evidence inset. Engram never persists transcript text.
