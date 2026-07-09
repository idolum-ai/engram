# Public Release Checklist

Engram is public-ready when:

- `make check` passes.
- `LICENSE` grants an OSI-style open source license.
- No live credentials, `.env` files, state files, logs, PEM files, generated
  captures, or downloaded attachments are tracked.
- The README first run is copy/paste-ready from BotFather setup through
  `/new pwd`, and uses the protected canonical env path.
- `.env.example` and the README configuration table contain the same supported
  settings without example secrets.
- README documents Telegram, Anthropic Haiku, tmux, local state and logs,
  attachments, downloads, redaction limits, and transcript sensitivity.
- Linux install, update, service, and uninstall steps are current; macOS is
  described as manual operation without built-in launchd support.
- `SECURITY.md` states supported versions and links to private GitHub
  vulnerability reporting.
- The bug template requests OS, Go, tmux, install mode, private DM chat type,
  diagnostics, and explicit evidence-redaction review.
- `README.md`, `SECURITY.md`, `CONTRIBUTING.md`, and
  `THIRD_PARTY_NOTICES.md` are current and use Engram terminology.
- Command metadata matches runtime command behavior.
- The requirements in `requirements/` describe the changed behavior.
