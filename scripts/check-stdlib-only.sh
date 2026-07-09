#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

module="$(go list -m)"
extra="$(go list -m all | awk -v module="$module" '$1 != module { print }')"
if [[ -n "$extra" ]]; then
  echo "Engram must remain Go stdlib-only; unexpected modules:" >&2
  echo "$extra" >&2
  exit 1
fi

echo "stdlib-only check passed"
