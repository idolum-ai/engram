# End-to-End Testing

Engram has two manually dispatched suites. The `hermetic` suite asks
whether the real Engram process can coordinate a Telegram-shaped exchange, an
isolated real tmux server, and a real local Chromium renderer as one usable
system. The `agent-ui` suite asks whether real agent CLIs produce equivalent
semantic state when configured to use local provider fixtures. The agent-UI
suite isolates credentials and application state, but it is not an OS-level
network sandbox.

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

## Agent UI semantics

Choose the `agent-ui` suite to run the real Codex, Claude Code, and OpenCode
clients. The trusted workflow installs explicit harness versions, gives each
client a private home, config, cache, work directory, and tmux server, and
routes the Responses, Messages, or OpenAI-compatible chat stream to a stdlib
loopback server. A loopback proxy rejects and records optional update,
telemetry, and discovery requests. The fixture uses only visibly fake keys and
does not read repository or runner credentials. Proxy variables do not prevent
a client from opening a direct socket, so this suite does not prove complete
egress isolation. The workflow pins the three requested top-level CLI versions;
their transitive npm dependencies are not lockfile-reproducible.

The equivalent local command is:

```sh
mkdir -p /tmp/engram-agent-ui-e2e
ENGRAM_AGENT_UI_E2E=1 \
  ENGRAM_AGENT_UI_REQUIRE_ALL=1 \
  ENGRAM_AGENT_UI_E2E_ARTIFACT_DIR=/tmp/engram-agent-ui-e2e \
  ENGRAM_AGENT_UI_CODEX="$(command -v codex)" \
  ENGRAM_AGENT_UI_CLAUDE="$(command -v claude)" \
  ENGRAM_AGENT_UI_OPENCODE="$(command -v opencode)" \
  ENGRAM_SNAPSHOT_BROWSER="$(command -v chromium)" \
  go test ./internal/e2e -run '^TestHermeticAgentUISemantics$' -count=1 -timeout=5m -v
```

Without `ENGRAM_AGENT_UI_REQUIRE_ALL=1`, unavailable clients are reported as
skipped so a developer can validate the clients installed on that host. No
client is downloaded by the Go test itself. The checked-in replay corpus under
`internal/agentui/testdata` remains part of ordinary `make check` and is the
fast regression gate.

On macOS, PNG evidence requires a dedicated `chrome-headless-shell`,
`chromium-headless-shell`, or `headless_shell` executable. Unset
`ENGRAM_SNAPSHOT_BROWSER` to run only the semantic assertions. The harness
refuses a full Chrome/Chromium app or wrapper because launching it can contend
with a live Engram service for renderer processes. The escape hatch
`ENGRAM_AGENT_UI_E2E_ALLOW_SHARED_BROWSER=1` is intended only for an isolated
host or after stopping the live service.

The suite retains, per client, active and idle plain-text captures, semantic
analysis JSON, observed client version, loopback request paths, proxy-rejected
request destinations, and a Chromium-rendered idle PNG. Its `manifest.json`
lists clients that passed or were unavailable. These artifacts contain fixture
content and private temporary paths, never request bodies, headers, tokens, or
the private temporary trees. See
[`agent-screen-semantics.md`](agent-screen-semantics.md) for the production
contract.

## Evidence

Every completed manually dispatched workflow test uploads a 30-day artifact
containing the files below. A local test writes the same evidence bundle to
`ENGRAM_E2E_ARTIFACT_DIR` without applying a retention policy.

- `snapshot.png`, the exact canonical terminal image uploaded to the simulator;
- `snapshot.txt`, the plain-text form of the same production-equivalent 64-row
  capture used for the image, for inspection and assistive technology;
- `transcript.html` and `transcript.png`, a phone-sized rendering of that card;
- `manifest.json`, the completed assertions, observed Telegram method counts,
  resolved Go, runner, tmux, and browser versions, and hashes binding the image
  and text evidence;
- `process.log`, streamed directly so output survives hard test termination;
- `supervisor.log`, retained cleanup-owner diagnostics;
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
