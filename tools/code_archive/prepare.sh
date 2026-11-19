#!/usr/bin/env bash
# Install the official v1.13.3 FFI and the selected grammars without modifying
# the Go module cache. Digests are from the upstream GitHub release asset API.
set -euo pipefail

archive_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
archive_version=1.13.3
archive_module_version="$(sed -nE 's@^[[:space:]]*github.com/xberg-io/tree-sitter-language-pack/packages/go[[:space:]]+v([^[:space:]]+).*@\1@p' "$archive_root/go.mod")"
[[ "$archive_module_version" == "$archive_version" ]] || { echo "Code archive native pin does not match go.mod" >&2; exit 1; }
for archive_tool in curl tar zstd; do
  command -v "$archive_tool" >/dev/null 2>&1 || { echo "Code archive preparation requires $archive_tool" >&2; exit 1; }
done
archive_destination="${1:-${archive_root}/deps/code_archive}"
archive_cache="${CODE_ARCHIVE_DOWNLOAD_CACHE:-${archive_root}/.cache/code_archive/v${archive_version}}"
archive_os="${RAGFLOW_TARGET_OS:-$(uname -s)}"
archive_arch="${RAGFLOW_TARGET_ARCH:-$(uname -m)}"
case "$archive_os" in Linux|linux) archive_os=linux;; Darwin|darwin) archive_os=macos;; *) echo "Unsupported code archive OS: $archive_os" >&2; exit 1;; esac
case "$archive_arch" in arm64|aarch64) archive_arch=arm64;; amd64|x86_64) archive_arch=x86_64;; *) echo "Unsupported code archive architecture: $archive_arch" >&2; exit 1;; esac
archive_platform="${archive_os}-${archive_arch}"
case "$archive_platform" in
  linux-arm64)
    archive_platform=linux-aarch64
    archive_ffi_sha=c86054f3f65cb1487a9b447677cc88c6a5c2ae0daabc326a737779bae652b1a6
    archive_parser_sha=e4c45c5804c9deb6bc4353962f5478160ca1abb219a745f455a8f56d04b0c1b6;;
  linux-x86_64)
    archive_ffi_sha=329dac8120699e599167d551b229f909c320d763f5a57159f1720f6096d1147b
    archive_parser_sha=30ffd923ed139943b0ccc60c0451f79ca232467f246372d84d88b2dba4e79122;;
  macos-arm64)
    archive_ffi_sha=b1f97f053606c84d151825e7480250c3b85256ba60658123a0a0cad507e02017
    archive_parser_sha=d982fa3ab89d9d710e2c002a7cd5a8bf8c8d6a6143ee27c4495e6a0ee70af387;;
  macos-x86_64)
    archive_ffi_sha=455745133ac325c990c50d4edd7e18a6ee4b381628803235b41f8e533200eaca
    archive_parser_sha=40fd0957a236df1c0d32a1e410b66f6affd71d09146021f1f035f99cc4ee8303;;
esac

archive_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

archive_fetch() {
  local name="$1" expected="$2" target="${archive_cache}/$1"
  if [[ -s "$target" && "$(archive_sha256 "$target")" == "$expected" ]]; then return; fi
  local partial="${target}.part.$$"
  local url="${CODE_ARCHIVE_ASSET_PREFIX:-}https://github.com/xberg-io/tree-sitter-language-pack/releases/download/v${archive_version}/${name}"
  if ! curl --fail --location --retry 2 --connect-timeout 20 --max-time 600 --output "$partial" "$url"; then
    rm -f "$partial"; return 1
  fi
  if [[ "$(archive_sha256 "$partial")" != "$expected" ]]; then
    rm -f "$partial"; echo "Checksum mismatch: $name" >&2; return 1
  fi
  mv "$partial" "$target"
}

mkdir -p "$archive_cache" "$archive_destination"
archive_ffi="tree-sitter-language-pack-go-v${archive_version}-${archive_platform}.tar.gz"
archive_parsers="parsers-${archive_platform}.tar.zst"
archive_fetch "$archive_ffi" "$archive_ffi_sha"
archive_fetch "$archive_parsers" "$archive_parser_sha"

archive_stage="$(mktemp -d "${archive_destination}/.prepare.XXXXXX")"
trap 'rm -rf "$archive_stage"' EXIT
mkdir -p "$archive_stage/ffi" "$archive_stage/grammars" "$archive_destination/native" "$archive_destination/grammars"
tar -xzf "$archive_cache/$archive_ffi" -C "$archive_stage/ffi"
archive_extension=so
[[ "$archive_os" == macos ]] && archive_extension=dylib
archive_library="$(find "$archive_stage/ffi" -type f -name "libts_pack_core_ffi.${archive_extension}" -print -quit)"
[[ -n "$archive_library" ]] || { echo "FFI library is absent from the pinned archive" >&2; exit 1; }
# Install only the shared FFI. The static archive also contains tree-sitter's C
# runtime, which collides with the runtime embedded by go-tree-sitter.
cp "$archive_library" "$archive_destination/native/libts_pack_core_ffi.${archive_extension}"

archive_members=()
for archive_language in python go java javascript typescript tsx vue html c cpp lua; do
  archive_members+=("./libtree_sitter_${archive_language}.${archive_extension}")
done
zstd -dc "$archive_cache/$archive_parsers" | tar -x -C "$archive_stage/grammars" "${archive_members[@]}"
for archive_language in python go java javascript typescript tsx vue html c cpp lua; do
  cp "$archive_stage/grammars/libtree_sitter_${archive_language}.${archive_extension}" "$archive_destination/grammars/"
done
printf 'version=%s\nplatform=%s\nffi_sha256=%s\nparsers_sha256=%s\n' "$archive_version" "$archive_platform" "$archive_ffi_sha" "$archive_parser_sha" > "$archive_destination/installed.lock"
printf 'Code archive v%s ready: %s\n' "$archive_version" "$archive_destination"
