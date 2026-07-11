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

required_release_phrases=(
  'startsWith(github.event.pull_request.head.ref, '\''release/v'\'')'
  'make release-dist'
  'ENGRAM_TMUX_INTEGRATION=1'
  'gh release create'
)
for phrase in "${required_release_phrases[@]}"; do
  if ! grep -R -F -- "${phrase}" .github/workflows/release*.yml >/dev/null; then
    echo "release workflows are missing required behavior: ${phrase}" >&2
    exit 1
  fi
done

echo "workflow sanity check passed"
