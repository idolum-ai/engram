#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
output_dir="${2:-dist}"

if ! [[ "${version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]]; then
  echo "release version must match vX.Y.Z or vX.Y.Z-prerelease; got '${version}'" >&2
  exit 2
fi

for command in go git tar; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "required command not found: ${command}" >&2
    exit 1
  }
done

repo_root="$(git rev-parse --show-toplevel)"
cd "${repo_root}"

release_commit="${RELEASE_COMMIT:-$(git rev-parse --short=12 HEAD)}"
release_date="${RELEASE_DATE:-$(git show -s --format=%cI HEAD)}"
source_epoch="${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD)}"
output_dir="$(mkdir -p "${output_dir}" && cd "${output_dir}" && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

ldflags="-s -w -X github.com/idolum-ai/engram/internal/version.Version=${version} -X github.com/idolum-ai/engram/internal/version.Commit=${release_commit} -X github.com/idolum-ai/engram/internal/version.Date=${release_date}"
assets=()
read -r -a targets <<< "${RELEASE_TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"

for target in "${targets[@]}"; do
  case "${target}" in
    linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) ;;
    *) echo "unsupported release target: ${target}" >&2; exit 2 ;;
  esac
  os="${target%/*}"
  arch="${target#*/}"
  asset="engram-${version}-${os}-${arch}.tar.gz"
  package_dir="${work_dir}/${os}-${arch}"
  mkdir -p "${package_dir}"
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" go build \
    -trimpath -buildvcs=false -ldflags "${ldflags}" \
    -o "${package_dir}/engram" ./cmd/engram
  chmod 0755 "${package_dir}/engram"
  cp README.md LICENSE "${package_dir}/"
  rm -f "${output_dir}/${asset}"
  if tar --version 2>/dev/null | grep -q 'GNU tar'; then
    tar --sort=name --mtime="@${source_epoch}" --owner=0 --group=0 --numeric-owner \
      -czf "${output_dir}/${asset}" -C "${package_dir}" engram README.md LICENSE
  else
    tar -czf "${output_dir}/${asset}" -C "${package_dir}" engram README.md LICENSE
  fi
  assets+=("${asset}")
done

(
  cd "${output_dir}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${assets[@]}" > checksums.txt
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${assets[@]}" > checksums.txt
  else
    echo "sha256sum or shasum is required" >&2
    exit 1
  fi
)

printf 'packaged %s at %s\n' "${version}" "${output_dir}"
