#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

secret_pattern='[0-9]{8,}:[A-Za-z0-9_-]{30,}|sk-ant-[A-Za-z0-9_-]{20,}|sk-(proj-)?[A-Za-z0-9_-]{20,}'

if git ls-files -z | xargs -0 -r rg -n "$secret_pattern" >/dev/null; then
  echo "possible secret in tracked files:" >&2
  git ls-files -z | xargs -0 -r rg -n "$secret_pattern" >&2
  exit 1
fi

echo "secret check passed"
