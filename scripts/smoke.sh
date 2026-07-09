#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

bin="${ENGRAM_SMOKE_BIN:-bin/engram}"

if [[ ! -x "$bin" ]]; then
  echo "missing smoke binary: $bin" >&2
  exit 1
fi

"$bin" version >/dev/null
"$bin" commands | rg -q '"command": "sessions"'
"$bin" commands | rg -q '"command": "attach"'

echo "smoke check passed"
