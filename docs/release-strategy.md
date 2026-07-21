# Release Strategy

Engram releases are small, reviewed snapshots of `main`. A release is not a
deployment and does not restart a running Engram service.

## Goals

- make the exact release source and human meaning reviewable before publication;
- publish portable binaries without introducing a release framework or Go dependency;
- give every asset an embedded version, commit, and build date;
- let users verify downloads before installation;
- keep tagging and GitHub Release creation inside narrowly scoped automation.

## Versions

Engram uses Semantic Versioning tags such as `v0.3.0`. Before `v1.0.0`, a minor
release may include a deliberate compatibility change, but its release notes
must call that out plainly. Patch releases are reserved for compatible fixes.
Prereleases may use a suffix such as `v0.3.0-rc.1`.

Version validation follows Semantic Versioning's numeric and prerelease rules:
numeric components and numeric prerelease identifiers do not accept leading
zeroes, empty identifiers are rejected, and build metadata is not used in tags.

The state schema has its own integer version. A product version and a state
schema version are different things: releases may leave the state schema
unchanged, and schema migrations must remain covered by their normal tests.

## Normal Release Path

1. Start `release/vX.Y.Z` from current `main`.
2. Move the accumulated entries under `Unreleased` in `CHANGELOG.md` into a
   dated `## [vX.Y.Z] - YYYY-MM-DD` section, then restore an empty `Unreleased`
   section at the top.
3. Generate a draft from git history when useful:

   ```sh
   ./scripts/generate-release-notes.sh --to HEAD --title "vX.Y.Z"
   ```

4. Open a pull request from `release/vX.Y.Z` into protected `main`. A non-empty
   PR body with `Summary`, `Compatibility`, and `Validation` sections is required
   and becomes the GitHub Release text. It should explain user-visible changes,
   operational changes, compatibility or migration notes, validation, and known
   limitations. Editing the PR body reruns candidate validation. A raw commit
   list or untouched pull-request template is not sufficient.
5. Wait for normal CI and the release-candidate workflow. The candidate workflow
   checks the changelog version, runs the full gate and hermetic real-tmux
   integration on Linux and Darwin, builds every release archive, and uploads
   an artifact preview with checksums and draft notes.
6. Review the PR and its candidate artifacts. Merge only when the release text
   and binaries describe the same source.
7. The release workflow verifies that the merged tree exactly matches the
   candidate-reviewed release-branch head, then repeats validation and builds
   fresh assets from the resulting `main` commit without write credentials.
8. A separate publication job receives write authority, creates the tag with an
   atomic create-only push, recovers or creates a draft release, uploads and
   verifies all assets, then publishes it. Maintainers do not push the normal
   release tag manually.

The release branch must be current with `main` before merge. The conventional
`release/vX.Y.Z` to `main` direction keeps the protected integration branch as
the source of truth. The release branch is short-lived and contains only release
preparation discovered during review. The tag points to the resulting `main`
commit, and publication fails closed if its tree differs from the reviewed
branch tree, regardless of merge method.

## Repository Prerequisites

Before the first release, maintainers configure and verify these GitHub settings:

- protected `main` requires current branches, required CI, and review;
- the `release` Actions environment requires maintainer approval;
- Actions may request `contents: write` only in the publication job;
- immutable releases are enabled for the repository;
- a `v*` tag ruleset prevents update and deletion while allowing the release
  environment's GitHub Actions token to create a new tag;
- Actions are restricted to reviewed actions pinned by full commit SHA.

The workflow and repository settings are complementary. The workflow refuses
tag replacement and publishes through a draft, but repository settings enforce
immutability after publication. Until those settings are enabled, immutability
is project policy rather than a technical guarantee.

## Published Assets

Each release contains:

- `engram-vX.Y.Z-linux-amd64.tar.gz`
- `engram-vX.Y.Z-linux-arm64.tar.gz`
- `engram-vX.Y.Z-darwin-amd64.tar.gz`
- `engram-vX.Y.Z-darwin-arm64.tar.gz`
- `checksums.txt`

Every archive contains `engram`, `README.md`, and `LICENSE`. Binaries are built
with `CGO_ENABLED=0`, `-trimpath`, and embedded release metadata. The release
workflow builds from the merged release PR commit, not from a maintainer
worktree or previously uploaded candidate artifact.

`checksums.txt` detects corruption and asset substitution after publication. It
does not protect against compromise of both the GitHub release and its checksum
file. Code signing and independent provenance attestations are useful future
layers, not claims made by the initial release pipeline.

## Installation And Updates

`scripts/install-release.sh` selects the current Linux or Darwin architecture,
downloads one archive and `checksums.txt`, verifies SHA-256, checks archive
contents, verifies the binary's embedded version, and atomically replaces
`~/.local/bin/engram` by default.

The installer does not create configuration, install a service, or restart one.
After an update, the operator chooses when to restart and can inspect the new
binary first with `engram version`. This preserves active tmux work and keeps
service interruption explicit. A systemd `Restart=on-failure` can activate the
new file after an unexpected crash, so operators needing a strict activation
boundary stop the service before replacement. After restart, Telegram
`/version` or `/status` and `systemctl --user is-active engram.service` verify
the running process.

Initial systemd setup still uses a source checkout for `.env.example` and the
unit template. `make install-service-unit` installs the unit around an existing
release binary; `make install-service` intentionally builds and installs from
the checkout first.

Source-checkout installation through `make install` remains supported for
development. It reports version `dev`; published release binaries report their
tag.

## Failure And Recovery

- Candidate failure: fix the release branch and let the PR checks rerun.
- Publication validation or build failure: fix `main`, create a new release
  branch/version when necessary, and do not manufacture assets locally.
- Workflow failure before publication: rerun the failed job. A matching tag and
  draft release are recovered; a matching complete release is verified and
  treated as success.
- A tag at another commit or a published release with missing or different
  assets fails closed. Investigate rather than overwriting it.
- Fault discovered after publication: leave the immutable release intact and
  publish a correcting version. Mark a dangerous release clearly in GitHub.

## Release Review Checklist

- [ ] Branch name, changelog heading, and proposed tag agree.
- [ ] The branch contains current `main` and only release preparation.
- [ ] PR notes explain impact, compatibility, and known limits.
- [ ] `make check` and the hermetic real-tmux integration test pass on Linux and
  Darwin.
- [ ] Candidate archives exist for all four supported OS/architecture pairs.
- [ ] A native candidate reports the intended version and commit.
- [ ] Checksums cover exactly the published archives.
- [ ] No runtime config, transcript, token, state, or generated local artifact is included.
- [ ] Publication and any live-service restart are understood as separate actions.
- [ ] The protected `release` environment, immutable releases, and `v*` tag ruleset are enabled.

## Non-Goals

- every merge to `main` becoming a release;
- mutable tags or replacement of published assets;
- automatic service restart or unattended update;
- package-manager distribution in the first iteration;
- claiming code signing, SBOMs, or independent build provenance before those
  surfaces are actually implemented and maintained.
