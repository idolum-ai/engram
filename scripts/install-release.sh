#!/usr/bin/env bash
set -euo pipefail

ENGRAM_RELEASE_REPO="${ENGRAM_RELEASE_REPO:-idolum-ai/engram}"
ENGRAM_INSTALL_DIR="${ENGRAM_INSTALL_DIR:-${HOME}/.local/bin}"

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

download() {
  local url="$1"
  local destination="$2"
  if command -v curl >/dev/null 2>&1; then
    curl --fail --location --silent --show-error --retry 3 --proto '=https' --tlsv1.2 \
      "${url}" --output "${destination}" || die "download failed: ${url}"
    return
  fi
  if command -v wget >/dev/null 2>&1; then
    wget --https-only --tries=3 --quiet --output-document="${destination}" "${url}" || \
      die "download failed: ${url}"
    return
  fi
  die "curl or wget is required"
}

normalize_os() {
  case "${1:-}" in
    Linux) printf 'linux\n' ;;
    Darwin) printf 'darwin\n' ;;
    *) die "unsupported operating system: ${1:-unknown}" ;;
  esac
}

normalize_arch() {
  case "${1:-}" in
    x86_64|amd64) printf 'amd64\n' ;;
    arm64|aarch64) printf 'arm64\n' ;;
    *) die "unsupported architecture: ${1:-unknown}" ;;
  esac
}

latest_version() {
  local destination="$1"
  download "https://api.github.com/repos/${ENGRAM_RELEASE_REPO}/releases/latest" "${destination}"
  sed -nE 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' "${destination}" | head -n1
}

checksum_file() {
  local path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${path}" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${path}" | awk '{print $1}'
  else
    die "sha256sum or shasum is required"
  fi
}

validate_version() {
  local candidate="$1" value core prerelease part identifier
  local -a parts identifiers
  [[ "${candidate}" == v* && "${candidate}" != *+* ]] || return 1
  value="${candidate#v}"
  if [[ "${value}" == *-* ]]; then
    core="${value%%-*}"
    prerelease="${value#*-}"
    [[ -n "${prerelease}" ]] || return 1
  else
    core="${value}"
    prerelease=""
  fi
  [[ "${core}" != .* && "${core}" != *. && "${core}" != *..* ]] || return 1
  IFS=. read -r -a parts <<< "${core}"
  [[ "${#parts[@]}" -eq 3 ]] || return 1
  for part in "${parts[@]}"; do
    [[ "${part}" =~ ^(0|[1-9][0-9]*)$ ]] || return 1
  done
  if [[ -n "${prerelease}" ]]; then
    [[ "${prerelease}" != .* && "${prerelease}" != *. && "${prerelease}" != *..* ]] || return 1
    IFS=. read -r -a identifiers <<< "${prerelease}"
    for identifier in "${identifiers[@]}"; do
      [[ -n "${identifier}" && "${identifier}" =~ ^[0-9A-Za-z-]+$ ]] || return 1
      if [[ "${identifier}" =~ ^[0-9]+$ && ! "${identifier}" =~ ^(0|[1-9][0-9]*)$ ]]; then
        return 1
      fi
    done
  fi
}

version="${1:-}"
for command in uname tar install awk sed grep mktemp; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

if [[ -z "${version}" ]]; then
  version="$(latest_version "${tmp_dir}/latest.json")"
fi
validate_version "${version}" || die "release version must be Semantic Versioning with a v prefix; got '${version}'"

os="$(normalize_os "$(uname -s)")"
arch="$(normalize_arch "$(uname -m)")"
asset="engram-${version}-${os}-${arch}.tar.gz"
base_url="https://github.com/${ENGRAM_RELEASE_REPO}/releases/download/${version}"
archive="${tmp_dir}/${asset}"
checksums="${tmp_dir}/checksums.txt"

download "${base_url}/${asset}" "${archive}"
download "${base_url}/checksums.txt" "${checksums}"

expected="$(awk -v asset="${asset}" '$2 == asset { print $1; exit }' "${checksums}")"
[[ -n "${expected}" ]] || die "checksum for ${asset} is missing"
actual="$(checksum_file "${archive}")"
[[ "${actual}" = "${expected}" ]] || die "checksum mismatch for ${asset}"

archive_entries="$(tar -tzf "${archive}" | LC_ALL=C sort)"
expected_entries="$(printf '%s\n' LICENSE README.md engram)"
[[ "${archive_entries}" = "${expected_entries}" ]] || \
  die "archive must contain exactly LICENSE, README.md, and engram"

tar -xzf "${archive}" -C "${tmp_dir}" engram
[[ -f "${tmp_dir}/engram" && ! -L "${tmp_dir}/engram" && -x "${tmp_dir}/engram" ]] || \
  die "archive did not contain a regular executable engram binary"
binary_version="$("${tmp_dir}/engram" version)"
printf '%s\n' "${binary_version}" | grep -F "engram ${version} " >/dev/null || \
  die "binary version does not match ${version}"

mkdir -p "${ENGRAM_INSTALL_DIR}"
[[ ! -d "${ENGRAM_INSTALL_DIR}/engram" ]] || die "install target is a directory: ${ENGRAM_INSTALL_DIR}/engram"
install_tmp="$(mktemp "${ENGRAM_INSTALL_DIR}/.engram-install.XXXXXX")"
trap 'rm -rf "${tmp_dir}"; rm -f "${install_tmp:-}"' EXIT
install -m 0755 "${tmp_dir}/engram" "${install_tmp}"
mv -f "${install_tmp}" "${ENGRAM_INSTALL_DIR}/engram"
[[ -f "${ENGRAM_INSTALL_DIR}/engram" && ! -L "${ENGRAM_INSTALL_DIR}/engram" && -x "${ENGRAM_INSTALL_DIR}/engram" ]] || \
  die "installed binary did not become a regular executable"

printf 'Installed %s to %s/engram\n' "${version}" "${ENGRAM_INSTALL_DIR}"
printf 'The service was not restarted. Verify with: %s/engram version\n' "${ENGRAM_INSTALL_DIR}"
