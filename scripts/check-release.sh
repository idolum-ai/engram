#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

if ./scripts/package-release.sh 1.2.3 "${tmp_dir}/invalid" >/dev/null 2>&1; then
  echo "release packager accepted an invalid version" >&2
  exit 1
fi

host_os="$(go env GOOS)"
host_arch="$(go env GOARCH)"
target="${host_os}/${host_arch}"
case "${target}" in
  linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) ;;
  *) echo "release smoke does not support host target ${target}" >&2; exit 1 ;;
esac

version="v0.0.0-check"
commit="releasecheck"
asset="engram-${version}-${host_os}-${host_arch}.tar.gz"
RELEASE_TARGETS="${target}" \
RELEASE_COMMIT="${commit}" \
RELEASE_DATE="1970-01-01T00:00:00Z" \
SOURCE_DATE_EPOCH=0 \
  ./scripts/package-release.sh "${version}" "${tmp_dir}/dist" >/dev/null

[[ -f "${tmp_dir}/dist/${asset}" ]] || { echo "release smoke archive is missing" >&2; exit 1; }
[[ "$(wc -l < "${tmp_dir}/dist/checksums.txt")" -eq 1 ]] || {
  echo "release smoke checksums did not contain exactly one asset" >&2
  exit 1
}
grep -F "  ${asset}" "${tmp_dir}/dist/checksums.txt" >/dev/null || {
  echo "release smoke checksum omitted ${asset}" >&2
  exit 1
}

entries="$(tar -tzf "${tmp_dir}/dist/${asset}" | LC_ALL=C sort)"
expected_entries="$(printf '%s\n' LICENSE README.md engram)"
[[ "${entries}" = "${expected_entries}" ]] || {
  echo "release smoke archive contents are incorrect" >&2
  exit 1
}

tar -xzf "${tmp_dir}/dist/${asset}" -C "${tmp_dir}" engram
"${tmp_dir}/engram" version | grep -F "engram ${version} commit=${commit}" >/dev/null || {
  echo "release smoke binary metadata is incorrect" >&2
  exit 1
}

./scripts/generate-release-notes.sh \
  --from HEAD --to HEAD --output "${tmp_dir}/notes.md" --title "${version}" >/dev/null
grep -F "# ${version}" "${tmp_dir}/notes.md" >/dev/null || {
  echo "release notes preview omitted its title" >&2
  exit 1
}

echo "release tooling check passed"
