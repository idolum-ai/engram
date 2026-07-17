#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ -d .github/workflows ]]; then
  if find .github/workflows -type f \( -name '*.yml' -o -name '*.yaml' \) -print0 | xargs -0 -r grep -n $'\t'; then
    echo "workflow files must not contain tabs" >&2
    exit 1
  fi
fi

if git grep -n -E '^(<<<<<<<|=======|>>>>>>>)' -- . ':!bin' >/dev/null; then
  echo "merge conflict marker found" >&2
  git grep -n -E '^(<<<<<<<|=======|>>>>>>>)' -- . ':!bin' >&2
  exit 1
fi

if [[ -d scripts ]]; then
  bash -n scripts/*.sh
fi

required_candidate_phrases=(
  'persist-credentials: false'
  './scripts/prepare-release-notes.sh'
  'make release-dist'
  'ENGRAM_TMUX_INTEGRATION=1'
  'actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02'
)
for phrase in "${required_candidate_phrases[@]}"; do
  if ! grep -F -- "${phrase}" .github/workflows/release-candidate.yml >/dev/null; then
    echo "release candidate workflow is missing required behavior: ${phrase}" >&2
    exit 1
  fi
done

required_publish_phrases=(
  'environment: release'
  'contents: write'
  'persist-credentials: false'
  'merged release tree differs from the candidate-reviewed head'
  'git push origin "${SOURCE_SHA}:refs/tags/${TAG}"'
  '--verify-tag --draft'
  'gh release upload'
  '--draft=false'
)
for phrase in "${required_publish_phrases[@]}"; do
  if ! grep -F -- "${phrase}" .github/workflows/release.yml >/dev/null; then
    echo "release publication workflow is missing required behavior: ${phrase}" >&2
    exit 1
  fi
done

required_e2e_phrases=(
  'workflow_dispatch:'
  'target_ref:'
  'target_sha:'
  'test "${GITHUB_REF}" = "refs/heads/main"'
  'persist-credentials: false'
  'refs/heads/${TARGET_REF}:refs/remotes/origin/e2e-target'
  'runs-on: ubuntu-24.04'
  'go-version: 1.22.12'
  'ENGRAM_E2E=1'
  '.supervisor-done'
  "go test ./internal/e2e -run '^TestHermeticGoldenPath$'"
  'if-no-files-found: error'
  'actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02'
)
for phrase in "${required_e2e_phrases[@]}"; do
  if ! grep -F -- "${phrase}" .github/workflows/e2e.yml >/dev/null; then
    echo "manual E2E workflow is missing required behavior: ${phrase}" >&2
    exit 1
  fi
done

if grep -R -E 'uses:[[:space:]]+actions/(checkout|setup-go|upload-artifact|download-artifact)@v[0-9]+' \
  .github/workflows >/dev/null; then
  echo "workflows must pin official actions by full commit SHA" >&2
  exit 1
fi

echo "workflow sanity check passed"
