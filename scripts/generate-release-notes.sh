#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf '%s\n' 'usage: scripts/generate-release-notes.sh [--from REF] [--to REF] [--output PATH] [--title TEXT]' >&2
}

from_ref=""
to_ref="HEAD"
output_path=""
title="Release notes draft"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --from) [[ $# -ge 2 ]] || { usage; exit 2; }; from_ref="$2"; shift 2 ;;
    --to) [[ $# -ge 2 ]] || { usage; exit 2; }; to_ref="$2"; shift 2 ;;
    --output) [[ $# -ge 2 ]] || { usage; exit 2; }; output_path="$2"; shift 2 ;;
    --title) [[ $# -ge 2 ]] || { usage; exit 2; }; title="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; usage; exit 2 ;;
  esac
done

repo_root="$(git rev-parse --show-toplevel)"
cd "${repo_root}"
to_commit="$(git rev-parse --verify "${to_ref}^{commit}")"
if [[ -z "${from_ref}" ]]; then
  from_ref="$(git describe --tags --abbrev=0 "${to_commit}^" 2>/dev/null || true)"
  if [[ -z "${from_ref}" ]]; then
    from_ref="$(git rev-list --max-parents=0 "${to_commit}" | tail -n1)"
  fi
fi
from_commit="$(git rev-parse --verify "${from_ref}^{commit}")"
range="${from_commit}..${to_commit}"

emit() {
  printf '# %s\n\n' "${title}"
  printf 'Range: `%s..%s`\n\n' "$(git rev-parse --short "${from_commit}")" "$(git rev-parse --short "${to_commit}")"
  printf '## What changed\n\n'
  if ! git log --first-parent --reverse --format='- %s (`%h`)' "${range}"; then
    return 1
  fi
  printf '\n## Compatibility and operations\n\n'
  printf -- '- Review state migrations, configuration changes, and service restart needs.\n'
  printf '\n## Validation\n\n'
  printf -- '- [ ] `make check`\n'
  printf -- '- [ ] Real-tmux integration\n'
  printf -- '- [ ] Release archives and checksums inspected\n'
}

if [[ -n "${output_path}" ]]; then
  mkdir -p "$(dirname "${output_path}")"
  emit > "${output_path}"
  printf 'wrote %s\n' "${output_path}"
else
  emit
fi
