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

The harness gives tmux a private socket root, private home and configuration
roots, a deterministic shell identity, and a wrapper that invokes the absolute
tmux binary with `-f /dev/null`. A planted user configuration is part of the
fixture and the test proves it was ignored. The private server PID is tracked,
normally stopped through `kill-server`, and verified gone. An external stdlib
supervisor owns Engram and tmux cleanup through a control pipe, so Go test
timeouts and hard parent exits cannot strand either resource. The Telegram
simulator listens only inside the test process, uses distinct fixture user and
chat identifiers, and rejects wrong destinations, unknown messages, malformed
media edits, and reply targets that Telegram would reject. Requested polling
offsets are retained so the test can detect replayed effects.

## Running It

From GitHub, open **Actions**, select **Manual E2E**, choose **Run workflow**,
and select `main` as the workflow branch. Enter the complete 40-character
lowercase commit SHA to test, enter the same-repository branch whose current tip
is that SHA, and keep the `hermetic` suite selected. The trusted workflow
definition always comes from `main`; it fetches that real branch ref, verifies
its tip, and only then detaches at the requested commit. Do not select a
pull-request branch in the workflow branch menu.

The target commit must be the tip of a branch in `idolum-ai/engram`. Hidden
pull-request refs and fork-only commits cannot satisfy this check. Review them
locally or deliberately mirror the reviewed commit into a same-repository
branch first; never work around the boundary by dispatching workflow YAML from
an untrusted fork or PR branch.

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

Every completed workflow test uploads a 30-day artifact containing the files
below. A local test writes the same evidence bundle to
`ENGRAM_E2E_ARTIFACT_DIR` without applying a retention policy.

- `snapshot.png`, the exact canonical terminal image uploaded to the simulator;
- `snapshot.txt`, the plain-text form of the same production-equivalent 64-row
  capture used for the image, for inspection and assistive technology;
- `transcript.html` and `transcript.png`, a phone-sized rendering of that card;
- `manifest.json`, the completed assertions, observed Telegram method counts,
  resolved Go, runner, tmux, and browser versions, and hashes binding the image
  and text evidence;
- `process.log`, streamed directly so output survives hard test termination;
- `test.log` for workflow runs, including failed runs; and
- `telegram.log` when a failed test needs simulator diagnostics.

Failed test invocations write a failed manifest before returning. Missing
evidence is itself a workflow error. Dispatch validation or checkout failures
can occur before the test has an artifact directory and remain visible in the
workflow log.

These files are review evidence, not automatically approved documentation.
Promoting an image into the README requires a human to inspect the complete
artifact and deliberately copy the chosen asset into the repository.

The trusted workflow references no repository secrets or environment and never
contacts Telegram, Anthropic, or OpenAI. It does not upload the generated
`.env`, Engram state, audit log, tmux socket, browser profile, or raw temporary
directory. A separate real-Telegram lane, if ever justified, must have its own
threat model and approval boundary rather than silently extending this
workflow.
