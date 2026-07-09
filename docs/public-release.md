# Public Release Checklist

Engram is public-ready when:

- `make check` passes.
- No live credentials, `.env` files, state files, logs, PEM files, or downloaded
  attachments are tracked.
- `README.md`, `SECURITY.md`, `CONTRIBUTING.md`, and `THIRD_PARTY_NOTICES.md`
  are current.
- Command metadata matches runtime command behavior.
- The requirements in `requirements/` describe the changed behavior.
