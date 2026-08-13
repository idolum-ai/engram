# Data Flow And Privacy

This document explains what Engram sends, stores, and executes. The binding
security contract is [`requirements/security.md`](../requirements/security.md).

## Trust Boundary

Engram is a single-user bridge between one authorized Telegram chat and tmux
running as the same local OS user. Control of that Telegram account can become
shell access to the Engram account. A stolen bot token can expose bot traffic,
impersonate replies, or disrupt polling.

Telegram bot chats are not end-to-end encrypted. Use a dedicated bot in a
private DM. Do not place Engram in a group.

## External Transfers

| Destination | Trigger | Content |
| --- | --- | --- |
| Telegram Bot API | Service polling and every Telegram reply | Bot token in the API route, commands, guides, images, requested files, bounded logs, and control metadata |
| Selected guide provider | Guide refresh or one-off `🗣️ Talk` | Bounded current terminal text that is not credential-redacted and, when separately enabled and proven, bounded visible agent-session history that is redacted before delivery |
| OpenAI transcription | A replied voice note while `VOICE_INPUT_MODE=transcribe` | The selected voice-note audio; the resulting bounded transcript is sent to tmux |
| GitHub API | Approved `engram github exec` or `grant` request | GitHub App authentication, installation/repository inspection, and token minting for the exact requested ceiling |
| Approved child process | Successful GitHub token delivery | `GH_TOKEN` and `GITHUB_TOKEN` in the child environment; Engram does not print or persist the token |

Snapshot rendering is local. Chromium receives a private local HTML document
containing the bounded frame. The resulting image leaves the host only when it
is sent to Telegram.

Setting `TELEGRAM_API_BASE` to HTTP is accepted for local Bot API servers but
exposes credentials and content in transit. HTTPS is the normal boundary.

## Guide Input

The current terminal frame sent to the selected guide provider is bounded but
not credential-redacted. Separately admitted agent-session history is redacted
before provider delivery, and returned model prose is redacted before
persistence or Telegram delivery. Terminal text remains untrusted data. The
prompt asks the model to ignore instructions addressed to Engram or the reader,
but injection resistance is best effort. Model output is presentation and is
never executed automatically.

Engram computes session IDs, tmux identities, working directories, file and URL
references, hashes, timestamps, process facts, and geometry locally. Model
output cannot establish those facts.

Codex and Claude Code history require separate opt-ins. Engram admits visible
user and assistant text only after proving the exact current pane binding,
process incarnation, session identity, transcript, and supported parser. It
excludes hidden reasoning, system and developer messages, tools and tool
results, attachments, sidechains, subagent transcripts, and unknown record
shapes. History can explain the prior topic; current terminal facts still come
from the frame.

## Files And Captures

- Incoming Telegram attachments are stored under Engram's private runtime
  root. The configured soft limit can be bypassed once by an exact SHA-256 up to
  Telegram's 20 MiB cloud download limit and available disk.
- `/download` uploads one absolute, already-opened regular file through a
  bounded private snapshot. It rejects symlinks and files above Telegram's
  50 MiB cloud upload limit.
- `/raw` uploads a fresh bounded plain-text frame. `/dump` uploads retained tmux
  history. Neither should be treated as redacted.
- `/templates export` uploads the exact remembered input bodies.
- Engram limits queued and concurrent transfers. A failed transfer leaves the
  source file unchanged.

Paths supplied by terminal text, Telegram, or model output do not bypass file
validation. Generated private snapshot names are not exposed in Telegram.

## Local Storage

`ENGRAM_HOME`, normally `~/.engram`, contains:

- `state.json`: Telegram identifiers, watches, presentation state, recovery
  metadata, and bounded routing records;
- `audit.jsonl`: bounded operational events after pattern-based redaction;
- `templates.json`: exact user-authored template bodies in plaintext;
- `github-apps.json`: encrypted GitHub App enrollment records; and
- `github-device-seal.key`, when approval-only enrollment is used.

The private runtime root contains attachments, rendered captures, broker
sockets, process identity, and other bounded runtime artifacts. Engram prefers
a valid private `XDG_RUNTIME_DIR`; otherwise it uses a UID-specific directory
under the system temporary directory.

State and runtime files may contain sensitive data. Pattern-based redaction can
miss unknown credential formats and ordinary private prose. It does not sanitize
terminal captures, state, templates, downloaded files, Telegram history, or
provider requests after the documented redaction boundary.

## GitHub Credentials

Passphrase enrollment encrypts a GitHub App PEM in the vault. Approval-only
enrollment separates the encrypted vault from an owner-only device seal, which
is convenient but does not protect against compromise of the local account.
Configured-PEM unlock keeps the source PEM on disk and validates it at each
approval or use boundary.

Approval allows inspection and minting to continue. GitHub may still reject the
installation, repository, or permission scope. A minted token allows a child
request within its ceiling; it does not prove the child succeeded or changed a
repository. Exact-command leases and renewable work-session grants retain these
distinctions. See the [GitHub capability guide](github-app-capabilities.md).

## Local Effects

Authorized Telegram input can create tmux windows, execute shell commands, send
literal text or keys, change directories, rename watches, and close
Engram-created windows. Closing an attached pane only removes Engram's watch;
it does not kill the external tmux window.

`engram inspect` and `engram doctor agent` construct no Telegram, provider, or
Chromium clients. They emit bounded control-safe local text and do not mutate
Engram state or tmux. The frame inspector does not redact terminal content, and
tmux itself may run hooks configured by the local user.

## Operator Checklist

- Keep `~/.engram` mode `0700` and its secret files mode `0600` or stricter.
- Review `/download`, `/raw`, `/dump`, template exports, and compatibility
  fixtures before sharing them.
- Revoke exposed bot, provider, or GitHub credentials immediately.
- Use `engram github revoke` when a pane's bounded GitHub work is complete.
- Run `make secrets` before publishing repository changes.
- Report suspected vulnerabilities through the private route in
  [`SECURITY.md`](../SECURITY.md).
