# Model Evaluations

Engram's default gate is deterministic and credential-free. Provider-backed
evaluations are manual because pull requests do not receive model credentials.
They supplement focused unit and contract tests; they do not replace them.

## Guide Fixtures

The guide corpus checks factual fidelity, material-concept coverage, prompt
injection, contradictory negation, unsupported numbers, output bounds, and
truncation. Each case runs twice by default; `ENGRAM_LIVE_HAIKU_REPEATS=1..5`
changes the sample count.

```sh
ENGRAM_LIVE_HAIKU_EVAL=1 \
ANTHROPIC_API_KEY=... \
go test -v ./internal/anthropic \
  -run TestLiveHaikuConversationEvaluation -count=1
```

`ANTHROPIC_MODEL` may select the other admitted Haiku identifier.

## Incremental Guide Fixtures

The incremental corpus presents complete current evidence plus a prior
rendering and deterministic terminal changes. It covers completion, a new
blocker, stale-prose correction, and a warning that disappeared. Current frame
evidence remains authoritative.

```sh
ENGRAM_LIVE_HAIKU_INCREMENTAL_EVAL=1 \
ANTHROPIC_API_KEY=... \
go test -v ./internal/anthropic \
  -run TestLiveHaikuIncrementalConversationEvaluation -count=1
```

## Key Composer

The provider-neutral corpus covers exact sequences, safe clarification,
multilingual phrasing, transcription noise, negation, quoted and conditional
instructions, retractions, and prompt injection. The live gate rejects every
wrong executable sequence and requires safety cases to clarify or fail local
validation. It reports exact matches separately from conservative
clarifications and requires at least 80 percent useful outcomes.

```sh
ENGRAM_LIVE_KEYSEQ_EVAL=all \
ENGRAM_LIVE_KEYSEQ_EVAL_TRIALS=3 \
go test -v ./internal/keyseqeval \
  -run TestLiveKeyInterpretation -count=1
```

Provider credentials for the selected adapters must be present in the process
environment. Malformed output, provider errors, and transport failures fail the
evaluation.

## Blinded Prompt Tournament

The tournament gives production and challenger prompts identical evidence at
the production temperature. Candidate order rotates, names become fresh opaque
IDs, and an independent judge scores fidelity, usefulness, voice, and
readability from JSON-serialized untrusted evidence.

The human reference guides priority and style but cannot override terminal
truth. Contradictions, unsafe relayed instructions, unsupported numbers, and
output-bound violations fail independently of the judge. Acceptance requires
the production candidate to average at least 4/5 for blinded fidelity,
usefulness, and overall quality. Full-frame, preference-regression, and
continuation cohorts remain separate in the report.

```sh
ENGRAM_LIVE_HAIKU_TOURNAMENT=1 \
ENGRAM_TOURNAMENT_JUDGE_MODEL=claude-sonnet-4-6 \
ENGRAM_TOURNAMENT_PROMPT_FILE=/tmp/challenger-prompt.txt \
ANTHROPIC_API_KEY=... \
go test -v ./internal/anthropic \
  -run TestLiveHaikuPromptTournament -count=1
```

`ENGRAM_TOURNAMENT_CASES` selects a comma-separated set of exact fixture names.
The three preference fixtures are development-informed regression cases, not
an unseen generalization set. Preserve new user examples untouched when they
are intended as holdouts.

The judge has a separate adversarial probe. It places conflicting instructions
in the terminal evidence, preferred outcome, and candidate strings and requires
the grounded candidate to score higher:

```sh
ENGRAM_LIVE_TOURNAMENT_JUDGE_INJECTION=1 \
ENGRAM_TOURNAMENT_JUDGE_MODEL=claude-sonnet-4-6 \
ANTHROPIC_API_KEY=... \
go test -v ./internal/anthropic \
  -run TestLiveTournamentJudgeResistsInjectedEvidence -count=1
```

## Luna Adapter

The Luna compatibility check exercises the exact production request and
response path once:

```sh
ENGRAM_LIVE_LUNA_TEST=1 \
OPENAI_API_KEY=... \
go test -v ./internal/openai \
  -run TestLiveLunaCompatibility -count=1
```

## Interpretation

Live results are sampled evidence about one model version, request shape, and
fixture set. Preserve hard deterministic failures separately from judge scores.
Record the model identifiers, repeat counts, selected cases, prompt commit, and
raw test output before comparing runs. A passing model evaluation does not
weaken the runtime rule that model output is presentation rather than authority.
