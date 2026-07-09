#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

required_files=(
  LICENSE
  SECURITY.md
  CONTRIBUTING.md
  THIRD_PARTY_NOTICES.md
  README.md
  .env.example
  .gitleaks.toml
  .github/pull_request_template.md
  .github/ISSUE_TEMPLATE/bug_report.md
  .github/ISSUE_TEMPLATE/feature_request.md
  docs/public-release.md
)

for file in "${required_files[@]}"; do
  if [[ ! -f "$file" ]]; then
    echo "missing public-readiness file: $file" >&2
    exit 1
  fi
done

for forbidden in .env bin/engram; do
  if git ls-files --error-unmatch "$forbidden" >/dev/null 2>&1; then
    echo "forbidden tracked file: $forbidden" >&2
    exit 1
  fi
done

artifact_path_pattern='(^|/)(secrets?|private)(/|$)|\.(db|sqlite|sqlite3|log|pem|key)$|(^|/)\.env$'
if git ls-files | rg -n "$artifact_path_pattern" >/dev/null; then
  echo "tracked file looks like a private runtime artifact:" >&2
  git ls-files | rg -n "$artifact_path_pattern" >&2
  exit 1
fi

private_pattern='/home/[[:alnum:]_]+_gmail_com|[[:alnum:]._%+-]+@idolum\.ai'
if git ls-files | xargs -r rg -n "$private_pattern" >/dev/null; then
  echo "tracked files contain private workstation, account, or secret markers:" >&2
  git ls-files | xargs -r rg -n "$private_pattern" >&2
  exit 1
fi

echo "public readiness check passed"
