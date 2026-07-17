#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
dist_dir="${2:-dist}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

version="$("${script_dir}/validate-release-version.sh" "${version}")"
platform="${ENGRAM_SIGNING_PLATFORM:-$(uname -s)}"
[[ "${platform}" == "Darwin" ]] || {
  echo "Darwin release signing must run on macOS" >&2
  exit 1
}

signing_identity="${ENGRAM_MACOS_SIGNING_IDENTITY:-Developer ID Application}"
code_identifier="${ENGRAM_MACOS_CODE_IDENTIFIER:-ai.idolum.engram}"
notary_key="${ENGRAM_NOTARY_KEY:-}"
notary_key_id="${ENGRAM_NOTARY_KEY_ID:-}"
notary_issuer="${ENGRAM_NOTARY_ISSUER:-}"

[[ -n "${notary_key}" && -f "${notary_key}" ]] || { echo "ENGRAM_NOTARY_KEY must name the App Store Connect team key" >&2; exit 1; }
[[ -n "${notary_key_id}" ]] || { echo "ENGRAM_NOTARY_KEY_ID is required" >&2; exit 1; }
[[ -n "${notary_issuer}" ]] || { echo "ENGRAM_NOTARY_ISSUER is required" >&2; exit 1; }

for command in codesign ditto tar xcrun; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "required command not found: ${command}" >&2
    exit 1
  }
done

tar_bin="${TAR:-}"
if [[ -z "${tar_bin}" ]]; then
  if command -v gtar >/dev/null 2>&1; then
    tar_bin="gtar"
  else
    tar_bin="tar"
  fi
fi
"${tar_bin}" --version 2>/dev/null | grep -q 'GNU tar' || {
  echo "GNU tar is required to rebuild signed release archives" >&2
  exit 1
}

repo_root="$(git rev-parse --show-toplevel)"
cd "${repo_root}"
dist_dir="$(cd "${dist_dir}" && pwd)"
[[ -f "${dist_dir}/checksums.txt" ]] || { echo "release checksums are missing" >&2; exit 1; }

if command -v sha256sum >/dev/null 2>&1; then
  (cd "${dist_dir}" && sha256sum -c checksums.txt)
else
  (cd "${dist_dir}" && shasum -a 256 -c checksums.txt)
fi

source_epoch="${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD)}"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
notary_dir="${work_dir}/notary"
mkdir -p "${notary_dir}"

expected_entries="$(printf '%s\n' LICENSE README.md engram)"
for arch in amd64 arm64; do
  asset="engram-${version}-darwin-${arch}.tar.gz"
  archive="${dist_dir}/${asset}"
  [[ -f "${archive}" ]] || { echo "Darwin release archive is missing: ${asset}" >&2; exit 1; }
  entries="$(tar -tzf "${archive}" | LC_ALL=C sort)"
  [[ "${entries}" == "${expected_entries}" ]] || { echo "Darwin release archive contents are invalid: ${asset}" >&2; exit 1; }

  package_dir="${work_dir}/${arch}"
  mkdir -p "${package_dir}" "${notary_dir}/${arch}"
  tar -xzf "${archive}" -C "${package_dir}"
  [[ -f "${package_dir}/engram" && -x "${package_dir}/engram" ]] || { echo "Darwin release binary is invalid: ${asset}" >&2; exit 1; }

  codesign --force --sign "${signing_identity}" --identifier "${code_identifier}" \
    --options runtime --timestamp "${package_dir}/engram"
  codesign --verify --strict --verbose=2 "${package_dir}/engram"
  codesign -d -r- "${package_dir}/engram" 2>"${package_dir}/requirement.txt"
  grep -F "identifier \"${code_identifier}\"" "${package_dir}/requirement.txt" >/dev/null || {
    echo "signed Darwin binary has an unexpected designated requirement: ${asset}" >&2
    exit 1
  }
  grep -F 'anchor apple generic' "${package_dir}/requirement.txt" >/dev/null || {
    echo "signed Darwin binary is not anchored to an Apple-trusted identity: ${asset}" >&2
    exit 1
  }
  codesign -d --verbose=4 "${package_dir}/engram" 2>"${package_dir}/signature.txt"
  grep -F "Identifier=${code_identifier}" "${package_dir}/signature.txt" >/dev/null
  grep -E 'flags=.*\(.*runtime.*\)' "${package_dir}/signature.txt" >/dev/null
  grep -E 'TeamIdentifier=.+$' "${package_dir}/signature.txt" | grep -Fv 'TeamIdentifier=not set' >/dev/null
  cp "${package_dir}/engram" "${notary_dir}/${arch}/engram"
done

notary_archive="${work_dir}/engram-${version}-darwin.zip"
ditto -c -k --keepParent "${notary_dir}" "${notary_archive}"
xcrun notarytool submit "${notary_archive}" \
  --key "${notary_key}" \
  --key-id "${notary_key_id}" \
  --issuer "${notary_issuer}" \
  --wait --output-format json | tee "${work_dir}/notary-result.json"
grep -Eq '"status"[[:space:]]*:[[:space:]]*"Accepted"' "${work_dir}/notary-result.json" || {
  echo "Apple notarization did not accept the Darwin release binaries" >&2
  exit 1
}

for arch in amd64 arm64; do
  asset="engram-${version}-darwin-${arch}.tar.gz"
  package_dir="${work_dir}/${arch}"
  rm -f "${dist_dir}/${asset}"
  "${tar_bin}" --sort=name --mtime="@${source_epoch}" --owner=0 --group=0 --numeric-owner \
    -czf "${dist_dir}/${asset}" -C "${package_dir}" engram README.md LICENSE
done

shopt -s nullglob
assets=("${dist_dir}"/engram-"${version}"-*.tar.gz)
[[ "${#assets[@]}" -gt 0 ]] || { echo "release archives are missing" >&2; exit 1; }
(
  cd "${dist_dir}"
  names=()
  for asset in "${assets[@]}"; do
    names+=("$(basename "${asset}")")
  done
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${names[@]}" > checksums.txt
  else
    shasum -a 256 "${names[@]}" > checksums.txt
  fi
)

printf 'signed and notarized %s Darwin release assets at %s\n' "${version}" "${dist_dir}"
