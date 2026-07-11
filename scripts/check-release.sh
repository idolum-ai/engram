#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

for version in v0.0.0 v1.2.3-rc.1 v1.2.3-alpha-7; do
  ./scripts/validate-release-version.sh "${version}" >/dev/null
done
for version in 1.2.3 v01.2.3 v1.02.3 v1.2.3. v1.2.3-01 v1.2.3-rc. v1.2.3-a..b v1.2.3+meta; do
  if ./scripts/validate-release-version.sh "${version}" >/dev/null 2>&1; then
    echo "release version validator accepted ${version}" >&2
    exit 1
  fi
  if ./scripts/install-release.sh "${version}" >/dev/null 2>&1; then
    echo "release installer accepted ${version}" >&2
    exit 1
  fi
done

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
RELEASE_ALLOW_NONDETERMINISTIC_TAR=1 \
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

mkdir -p "${tmp_dir}/mock-bin" "${tmp_dir}/install"
cat > "${tmp_dir}/mock-bin/curl" <<'MOCK_CURL'
#!/usr/bin/env bash
set -euo pipefail
url=""
destination=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) destination="$2"; shift 2 ;;
    https://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
cp "${ENGRAM_TEST_DIST}/${url##*/}" "${destination}"
MOCK_CURL
chmod 0755 "${tmp_dir}/mock-bin/curl"
PATH="${tmp_dir}/mock-bin:${PATH}" \
ENGRAM_TEST_DIST="${tmp_dir}/dist" \
ENGRAM_INSTALL_DIR="${tmp_dir}/install" \
  ./scripts/install-release.sh "${version}" >/dev/null
"${tmp_dir}/install/engram" version | grep -F "engram ${version} commit=${commit}" >/dev/null || {
  echo "release installer did not install the verified binary" >&2
  exit 1
}
rm -f "${tmp_dir}/install/engram"
mkdir "${tmp_dir}/install/engram"
if PATH="${tmp_dir}/mock-bin:${PATH}" ENGRAM_TEST_DIST="${tmp_dir}/dist" ENGRAM_INSTALL_DIR="${tmp_dir}/install" \
  ./scripts/install-release.sh "${version}" >/dev/null 2>&1; then
  echo "release installer accepted a directory as its binary target" >&2
  exit 1
fi
mkdir "${tmp_dir}/bad-dist" "${tmp_dir}/bad-install"
cp "${tmp_dir}/dist/${asset}" "${tmp_dir}/bad-dist/${asset}"
printf '%064d  %s\n' 0 "${asset}" > "${tmp_dir}/bad-dist/checksums.txt"
if PATH="${tmp_dir}/mock-bin:${PATH}" ENGRAM_TEST_DIST="${tmp_dir}/bad-dist" ENGRAM_INSTALL_DIR="${tmp_dir}/bad-install" \
  ./scripts/install-release.sh "${version}" >/dev/null 2>&1; then
  echo "release installer accepted a checksum mismatch" >&2
  exit 1
fi

mkdir "${tmp_dir}/notes-repo"
(
  cd "${tmp_dir}/notes-repo"
  git init -q
  git config user.name Engram
  git config user.email engram@example.test
  printf 'first\n' > history.txt
  git add history.txt
  git commit -q -m 'First release change'
  printf 'second\n' >> history.txt
  git commit -q -am 'Second release change'
  "${repo_root}/scripts/generate-release-notes.sh" --output notes.md --title "${version}" >/dev/null
  grep -F 'First release change' notes.md >/dev/null
  grep -F 'Second release change' notes.md >/dev/null
)

cat > "${tmp_dir}/body.md" <<'NOTES'
## Summary

This release makes remote terminal work more legible and preserves the reviewed source boundary.

## Compatibility

No state migration is required. Existing configuration remains compatible.

## Validation

The full gate, release package smoke, and real tmux integration passed.
NOTES
./scripts/validate-release-notes.sh "${version}" "${tmp_dir}/body.md"
./scripts/prepare-release-notes.sh "${version}" "Release ${version}" "https://example.test/pr/1" \
  "${tmp_dir}/body.md" "${tmp_dir}/prepared.md"
grep -F "# ${version}" "${tmp_dir}/prepared.md" >/dev/null
grep -F 'Release PR: [Release' "${tmp_dir}/prepared.md" >/dev/null

echo "release tooling check passed"
