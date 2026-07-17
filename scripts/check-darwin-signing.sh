#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT
version="v0.0.0-signing-check"

checksum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi
}

verify_checksums() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c checksums.txt
  else
    shasum -a 256 -c checksums.txt
  fi
}

mkdir -p "${tmp_dir}/dist" "${tmp_dir}/package" "${tmp_dir}/mock-bin"
printf 'binary\n' > "${tmp_dir}/package/engram"
chmod 0755 "${tmp_dir}/package/engram"
cp README.md LICENSE "${tmp_dir}/package/"

for target in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64; do
  tar -czf "${tmp_dir}/dist/engram-${version}-${target}.tar.gz" \
    -C "${tmp_dir}/package" engram README.md LICENSE
done
(
  cd "${tmp_dir}/dist"
  checksum ./*.tar.gz > checksums.txt
)
linux_checksum="$(checksum "${tmp_dir}/dist/engram-${version}-linux-amd64.tar.gz")"
printf 'notary-key\n' > "${tmp_dir}/AuthKey_TEST.p8"

cat > "${tmp_dir}/mock-bin/codesign" <<'MOCK_CODESIGN'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "--force" ]]; then
  printf 'signed-id=ai.idolum.engram\n' >> "${!#}"
elif [[ "$1" == "-d" && "$2" == "-r-" ]]; then
  printf '# designated => identifier "ai.idolum.engram" and anchor apple generic\n' >&2
elif [[ "$1" == "-d" ]]; then
  printf 'Identifier=ai.idolum.engram\nflags=0x10000(runtime)\nTeamIdentifier=TESTTEAM01\n' >&2
fi
MOCK_CODESIGN

cat > "${tmp_dir}/mock-bin/ditto" <<'MOCK_DITTO'
#!/usr/bin/env bash
set -euo pipefail
source="${@: -2:1}"
destination="${@: -1}"
tar -czf "${destination}" -C "$(dirname "${source}")" "$(basename "${source}")"
MOCK_DITTO

cat > "${tmp_dir}/mock-bin/xcrun" <<'MOCK_XCRUN'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == "notarytool" && "$2" == "submit" && -f "$3" ]]
printf '%s\n' "$*" > "${ENGRAM_TEST_XCRUN_LOG}"
printf '{"id":"test","status":"Accepted"}\n'
MOCK_XCRUN
chmod 0755 "${tmp_dir}/mock-bin/codesign" "${tmp_dir}/mock-bin/ditto" "${tmp_dir}/mock-bin/xcrun"

PATH="${tmp_dir}/mock-bin:${PATH}" \
ENGRAM_SIGNING_PLATFORM=Darwin \
ENGRAM_MACOS_SIGNING_IDENTITY='Developer ID Application: Test' \
ENGRAM_NOTARY_KEY="${tmp_dir}/AuthKey_TEST.p8" \
ENGRAM_NOTARY_KEY_ID=TESTKEY \
ENGRAM_NOTARY_ISSUER=00000000-0000-0000-0000-000000000000 \
ENGRAM_TEST_XCRUN_LOG="${tmp_dir}/xcrun.log" \
SOURCE_DATE_EPOCH=0 \
  ./scripts/sign-darwin-release.sh "${version}" "${tmp_dir}/dist" >/dev/null

[[ "$(checksum "${tmp_dir}/dist/engram-${version}-linux-amd64.tar.gz")" == "${linux_checksum}" ]] || {
  echo "Darwin signing modified a Linux release asset" >&2
  exit 1
}
for arch in amd64 arm64; do
  mkdir -p "${tmp_dir}/signed-${arch}"
  tar -xzf "${tmp_dir}/dist/engram-${version}-darwin-${arch}.tar.gz" \
    -C "${tmp_dir}/signed-${arch}" engram
  grep -F 'signed-id=ai.idolum.engram' "${tmp_dir}/signed-${arch}/engram" >/dev/null || {
    echo "Darwin ${arch} release binary was not signed" >&2
    exit 1
  }
done
grep -F 'notarytool submit' "${tmp_dir}/xcrun.log" >/dev/null
[[ "$(wc -l < "${tmp_dir}/dist/checksums.txt")" -eq 4 ]] || {
  echo "signed release checksums are incomplete" >&2
  exit 1
}
(cd "${tmp_dir}/dist" && verify_checksums >/dev/null)

echo "Darwin release signing check passed"
