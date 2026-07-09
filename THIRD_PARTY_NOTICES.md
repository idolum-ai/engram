# Third Party Notices

Engram currently uses only the Go standard library at runtime.

External services and tools used by deployment or operation:

- Telegram Bot API
- Anthropic Messages API
- tmux
- systemd user services

CI uses GitHub Actions and installs `ripgrep` and `tmux` from the runner package
repositories.
