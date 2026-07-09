# tmux Requirements

tmux is the source of terminal truth.

## Session Selection

- If `ENGRAM_TMUX_SESSION` is configured, Engram uses that tmux session and
  creates it if missing.
- If unset, Engram uses the first existing tmux session.
- If no tmux session exists, Engram creates `engram-<chat-id>`.

## Windows And Attachments

- A top-level non-command Telegram message creates a new tmux window.
- `/attach <target>` resolves an existing tmux target and tracks its active pane
  as an Engram session.
- `/sessions` shows native tmux sessions and windows plus attach buttons for
  untracked windows.

## Input

- Replying to an Engram anchor sends text to the tracked pane and submits it.
- Command input sends literal text, waits briefly, then sends tmux `Enter`.
- `/text` sends literal text without `Enter`.
- `/key` sends tmux key names and must reject empty/newline-containing keys.

## Capture

- Live anchors summarize visible pane capture through Haiku.
- `/raw` sends the visible pane capture as an attachment.
- `/dump` sends full scrollback as an attachment.

## Closing

- `/close <id>` kills the tracked tmux window and marks the Engram session
  closed.
- Closed sessions must not continue refreshing.
