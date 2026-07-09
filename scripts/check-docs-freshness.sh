#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

required_docs=(
  docs/design-principles.md
  docs/feature-matrix.md
  requirements/INDEX.md
  tmux-telegram-client-spec.md
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
for command in $(printf '%s\n' "$commands_json" | sed -n 's/.*"command": "\(.*\)",/\1/p'); do
  if [[ "$command" == "kill" ]]; then
    continue
  fi
  if ! rg -q "/$command\b" README.md tmux-telegram-client-spec.md requirements docs; then
    echo "command /$command is missing from docs" >&2
    exit 1
  fi
done

echo "docs freshness check passed"
