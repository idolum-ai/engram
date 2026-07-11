#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

required_docs=(
  docs/design-principles.md
  docs/protocol-posture.md
  docs/release-strategy.md
  docs/terminal-mechanics-boundary.md
  docs/terminal-mechanics-plan.md
  requirements/INDEX.md
)

for file in "${required_docs[@]}"; do
  if [[ ! -s "$file" ]]; then
    echo "missing or empty documentation file: $file" >&2
    exit 1
  fi
done

export GOCACHE="${GOCACHE:-/tmp/engram-go-build}"
export GOMODCACHE="${GOMODCACHE:-/tmp/engram-go-mod}"

commands_json="$(go run ./cmd/engram commands)"
if ! printf '%s\n' "$commands_json" | rg -q '"command": "help"'; then
  echo "command metadata is empty or missing /help" >&2
  exit 1
fi

echo "docs freshness check passed"
