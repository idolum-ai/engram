#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

required_docs=(
	CONTRIBUTING.md
	README.md
	SECURITY.md
	docs/README.md
	docs/agent-screen-semantics.md
	docs/agent-compatibility.md
	docs/claude-code-session-context.md
	docs/codex-session-context.md
	docs/compatibility-fixtures.md
	docs/conceptual-model.md
	docs/configuration.md
	docs/data-flow.md
	docs/design-principles.md
	docs/e2e-testing.md
	docs/github-app-capabilities.md
	docs/headless-operation.md
	docs/model-evaluations.md
	docs/product-surface.md
	docs/protocol-posture.md
	docs/release-strategy.md
	docs/telegram-commands.md
  docs/terminal-mechanics-boundary.md
	docs/terminal-mechanics-plan.md
	docs/session-controls.md
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

generated_commands="$(mktemp)"
trap 'rm -f "$generated_commands"' EXIT
go run ./cmd/engram commands --format markdown >"$generated_commands"
if ! diff -u docs/telegram-commands.md "$generated_commands"; then
	echo "docs/telegram-commands.md is stale; regenerate it with:" >&2
	echo "  go run ./cmd/engram commands --format markdown > docs/telegram-commands.md" >&2
	exit 1
fi

example_keys="$(sed -n -E 's/^([A-Z][A-Z0-9_]*)=.*/\1/p' .env.example | LC_ALL=C sort -u)"
documented_keys="$(sed -n '/<!-- config-table:start -->/,/<!-- config-table:end -->/p' docs/configuration.md \
	| sed -n -E 's/^\| `([A-Z][A-Z0-9_]*)`.*/\1/p' \
	| LC_ALL=C sort -u)"
if [[ "$example_keys" != "$documented_keys" ]]; then
	echo "supported configuration keys differ between .env.example and docs/configuration.md:" >&2
	diff -u <(printf '%s\n' "$example_keys") <(printf '%s\n' "$documented_keys") >&2 || true
	exit 1
fi

if rg -n '`/version`' README.md SECURITY.md CONTRIBUTING.md docs requirements; then
	echo "public docs refer to unsupported Telegram /version; use /status or local engram version explicitly" >&2
	exit 1
fi

provider_disclosures=(
	'The current terminal frame sent to the selected guide provider is bounded but not credential-redacted.'
	'Separately admitted agent-session history is redacted before provider delivery, and returned model prose is redacted before persistence or Telegram delivery.'
)
for file in README.md docs/configuration.md docs/data-flow.md; do
	document_text="$(tr '\n' ' ' <"$file")"
	for disclosure in "${provider_disclosures[@]}"; do
		if [[ "$document_text" != *"$disclosure"* ]]; then
			echo "missing required guide-provider disclosure in $file: $disclosure" >&2
			exit 1
		fi
	done
done

while IFS= read -r file; do
	while IFS= read -r target; do
		case "$target" in
			http://*|https://*|mailto:*|'#'*) continue ;;
		esac
		target="${target%%#*}"
		target="${target#<}"
		target="${target%>}"
		if [[ -z "$target" || "$target" == *' '* ]]; then
			continue
		fi
		resolved="$(dirname "$file")/$target"
		if [[ ! -e "$resolved" ]]; then
			echo "broken local Markdown link in $file: $target" >&2
			exit 1
		fi
	done < <(rg -o '\[[^]]+\]\([^)]+\)' "$file" \
		| sed -n -E 's/^\[[^]]+\]\(([^)]+)\)$/\1/p')
done < <(rg --files -g '*.md')

echo "docs freshness check passed"
