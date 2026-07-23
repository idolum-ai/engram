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
  requirements/upstream-signals.md
)

for file in "${required_requirements[@]}"; do
  if [[ ! -f "$file" ]]; then
    echo "missing requirement file: $file" >&2
    exit 1
  fi
done

contains_fixed() {
  local phrase="$1"
  shift
  if command -v rg >/dev/null 2>&1; then
    rg -qF "$phrase" "$@"
  else
    grep -RqF -- "$phrase" "$@"
  fi
}

contains_pattern() {
  local pattern="$1"
  shift
  if command -v rg >/dev/null 2>&1; then
    rg -n "$pattern" "$@"
  else
    grep -RnE -- "$pattern" "$@"
  fi
}

for phrase in "make check" "Command metadata" "tmux is the source" "Audit" "Exactly one Telegram user"; do
  if ! contains_fixed "$phrase" requirements README.md; then
    echo "required architecture phrase missing: $phrase" >&2
    exit 1
  fi
done

if contains_pattern 'github.com/idolum-ai/engram/internal/app' internal/telegram internal/tmux internal/anthropic internal/openai internal/guide internal/commands internal/terminalshot >/dev/null; then
  echo "leaf packages must not import internal/app" >&2
  exit 1
fi

if contains_pattern 'github.com/idolum-ai/engram/internal/telegram' internal/tmux internal/anthropic internal/openai internal/guide internal/config internal/inspect internal/mechanics internal/state internal/terminalshot >/dev/null; then
  echo "non-app core packages must not import telegram" >&2
  exit 1
fi

if contains_pattern 'github.com/idolum-ai/engram/internal/(anthropic|app|guide|openai|state|telegram|tmux)' internal/keyseq >/dev/null; then
  echo "key sequence contracts must remain provider- and authority-neutral" >&2
  exit 1
fi

if contains_pattern 'github.com/idolum-ai/engram/internal/(app|anthropic|inspect|state|telegram|terminalshot)' internal/mechanics >/dev/null; then
  echo "terminal mechanics may depend only on tmux and the standard library" >&2
  exit 1
fi

if contains_pattern '\.Tmux\.(ValidatePane|SendCommand|SendText|SendKeys|CaptureStyled|CaptureVisibleRaw|DumpScrollback|KillWindow)' internal/app >/dev/null; then
  echo "pane-bound app operations must cross internal/mechanics" >&2
  exit 1
fi

echo "architecture check passed"
