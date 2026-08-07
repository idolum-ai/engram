# Compatibility Fixtures

Live terminal and transcript material is private by default. The opt-in capture
command creates a local candidate that is structurally useful but cannot be
checked in without a human privacy review:

```sh
engram compatibility capture \
  --provider claude \
  --pane %4 \
  --out /tmp/engram-claude-candidate
```

The output must be a new absolute directory outside every Git worktree. Engram
captures at most 96 pane rows, retains only allow-listed product/version,
duration, model-token, and border grammar, and replaces every other row with a
placeholder. Transcript inspection emits only bounded record counts,
allow-listed root-key names, record types, and content-block types. It never
emits message values. The production parser reads one bounded turn in memory to
report transcript eligibility, then discards it. Recognized paths, emails, and
UUIDs fail the operation.

The directory contains:

- `frame.sanitized.txt` — the reduced visible grammar;
- `transcript-inventory.json` — structure without content values;
- `compatibility.json` — candidate contract names, not a support claim; and
- `REVIEW_REQUIRED` — the mandatory manual-review reminder.

Review every byte locally. Do not check in the directory itself. Copy only the
minimum reviewed artifacts into the provider's parallel fixture corpus, replace
all identifiers with obvious synthetic values, and add adversarial negative
fixtures. A maintainer must then deliberately add the exact version to the
corresponding screen registry or parser declaration. Capture never changes a
registry, state file, tmux pane option, service, or support matrix.

Before review, verify that no user names, host names, repository names, paths,
session IDs, prompts, responses, tool input/output, agent names, email addresses,
or credentials remain. Run `make check`, which includes the secret scanner and
the provider-neutral compatibility tests.
