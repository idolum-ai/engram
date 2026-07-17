# End-to-End Testing

Engram has one manually dispatched, hermetic golden path. It answers a narrow
question: can the real Engram process coordinate a Telegram-shaped exchange,
an isolated real tmux server, and a real local Chromium renderer as one usable
system?

## What It Exercises

The workflow builds Engram, starts a stdlib-only local Telegram Bot API
simulator, and launches the binary against private temporary state. A scripted
authorized user then:

1. sends a message that creates a tracked tmux window;
2. waits for its canonical anchor to become a pinned Chromium snapshot;
3. replies to that anchor and verifies the command reached the same pane;
4. taps refresh and verifies the canonical message is edited; and
5. taps a numbered file control and verifies the exact local bytes are sent.

The harness removes `TMUX` from the child environment and gives tmux a private
socket root. It cannot see or mutate the runner's ordinary tmux server. The
Telegram simulator listens only inside the test process and accepts only the
fixture token, user, and chat.

## Running It

From GitHub, open **Actions**, select **Manual E2E**, choose **Run workflow**,
select the branch to test, and keep the `hermetic` suite selected. Because the
workflow already exists on the default branch, it can be dispatched against a
pull-request branch before merge.

The equivalent local command is:

```sh
make build
mkdir -p /tmp/engram-e2e-artifacts
ENGRAM_E2E=1 \
  ENGRAM_E2E_BINARY="$PWD/bin/engram" \
  ENGRAM_E2E_ARTIFACT_DIR=/tmp/engram-e2e-artifacts \
  ENGRAM_SNAPSHOT_BROWSER="$(command -v chromium)" \
  go test ./internal/e2e -run '^TestHermeticGoldenPath$' -count=1 -v
```

Use `google-chrome` or another supported executable when `chromium` is not
installed. The test is skipped during ordinary `go test ./...` and `make check`.

## Evidence

Every dispatched run retains a 30-day artifact containing:

- `snapshot.png`, the exact canonical terminal image uploaded to the simulator;
- `transcript.html` and `transcript.png`, a phone-sized rendering of that card;
- `manifest.json`, the passed assertions and observed Telegram method counts;
- `process.log` only when the hermetic Engram process wrote output.

These files are review evidence, not automatically approved documentation.
Promoting an image into the README requires a human to inspect the complete
artifact and deliberately copy the chosen asset into the repository.

The workflow never reads repository secrets and never contacts Telegram,
Anthropic, or OpenAI. It does not upload the generated `.env`, Engram state,
audit log, tmux socket, browser profile, or raw temporary directory. A separate
real-Telegram lane, if ever justified, must have its own threat model and
approval boundary rather than silently extending this workflow.
