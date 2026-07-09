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

echo "workflow sanity check passed"
