#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

required_requirements=(
  requirements/INDEX.md
  requirements/telegram.md
  requirements/tmux.md
  requirements/reliability.md
  requirements/security.md
  requirements/operations.md
)

for file in "${required_requirements[@]}"; do
  if [[ ! -f "$file" ]]; then
    echo "missing requirement file: $file" >&2
    exit 1
  fi
done

for phrase in "make check" "Command metadata" "tmux is the source" "Audit" "Exactly one Telegram user"; do
  if ! rg -qF "$phrase" requirements README.md tmux-telegram-client-spec.md; then
    echo "required architecture phrase missing: $phrase" >&2
    exit 1
  fi
done

if rg -n 'github.com/idolum-ai/engram/internal/app' internal/telegram internal/tmux internal/anthropic internal/commands >/dev/null; then
  echo "leaf packages must not import internal/app" >&2
  exit 1
fi

if rg -n 'github.com/idolum-ai/engram/internal/telegram' internal/tmux internal/anthropic internal/config internal/state >/dev/null; then
  echo "non-app core packages must not import telegram" >&2
  exit 1
fi

echo "architecture check passed"
