#!/bin/sh

set -eu

repository="maful/inline"
release_base_url="${INLINE_RELEASE_BASE_URL:-https://github.com/${repository}/releases/latest/download}"

fail() {
  printf 'inline installer: %s\n' "$1" >&2
  exit 1
}

download() {
  source_url="$1"
  destination="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 --output "$destination" "$source_url"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$destination" "$source_url"
  else
    fail "curl or wget is required"
  fi
}

case "$(uname -s)" in
  Darwin) operating_system="darwin" ;;
  Linux) operating_system="linux" ;;
  *) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
  x86_64 | amd64) architecture="amd64" ;;
  arm64 | aarch64) architecture="arm64" ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

archive_name="inline_${operating_system}_${architecture}.tar.gz"
temporary_directory="$(mktemp -d "${TMPDIR:-/tmp}/inline-install.XXXXXX")"
data_home="${XDG_DATA_HOME:-${HOME}/.local/share}"
install_root="${INLINE_INSTALL_ROOT:-${data_home}/inline}"
bin_directory="${INLINE_BIN_DIR:-${HOME}/.local/bin}"
lock_directory="${install_root}/install.lock"
lock_acquired="false"
staged_release=""

cleanup() {
  rm -rf "$temporary_directory"
  if [ -n "$staged_release" ] && [ -d "$staged_release" ]; then
    rm -rf "$staged_release"
  fi
  if [ "$lock_acquired" = "true" ]; then
    rmdir "$lock_directory" 2>/dev/null || true
  fi
}
trap cleanup EXIT HUP INT TERM

mkdir -p "${install_root}/releases" "$bin_directory"
if [ -e "${install_root}/current" ] && [ ! -L "${install_root}/current" ]; then
  fail "${install_root}/current exists and is not a symbolic link"
fi
if [ -e "${bin_directory}/inline" ] && [ ! -L "${bin_directory}/inline" ]; then
  fail "${bin_directory}/inline already exists and is not a symbolic link"
fi
if ! mkdir "$lock_directory" 2>/dev/null; then
  fail "another installation is running; remove ${lock_directory} if it is stale"
fi
lock_acquired="true"

printf 'Downloading Inline for %s/%s...\n' "$operating_system" "$architecture"
download "${release_base_url}/${archive_name}" "${temporary_directory}/${archive_name}"
download "${release_base_url}/checksums.txt" "${temporary_directory}/checksums.txt"

expected_checksum="$(awk -v name="$archive_name" '$2 == name || $2 == "*" name { print $1; exit }' "${temporary_directory}/checksums.txt")"
[ -n "$expected_checksum" ] || fail "checksums.txt does not contain ${archive_name}"

if command -v sha256sum >/dev/null 2>&1; then
  actual_checksum="$(sha256sum "${temporary_directory}/${archive_name}" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
  actual_checksum="$(shasum -a 256 "${temporary_directory}/${archive_name}" | awk '{ print $1 }')"
else
  fail "sha256sum or shasum is required"
fi

[ "$actual_checksum" = "$expected_checksum" ] || fail "checksum mismatch for ${archive_name}"
printf 'Checksum verified.\n'

archive_contents="$(tar -tzf "${temporary_directory}/${archive_name}")"
case "$archive_contents" in
  inline | ./inline) ;;
  *) fail "release archive has unexpected contents" ;;
esac

mkdir "${temporary_directory}/extracted"
tar -xzf "${temporary_directory}/${archive_name}" -C "${temporary_directory}/extracted"
binary_path="${temporary_directory}/extracted/inline"
[ -f "$binary_path" ] || binary_path="${temporary_directory}/extracted/./inline"
[ -f "$binary_path" ] || fail "release archive does not contain inline"
chmod 0755 "$binary_path"

installed_version="$($binary_path version --short)"
case "$installed_version" in
  "" | *[!0-9A-Za-z.+-]*) fail "release returned an invalid version" ;;
esac

release_name="v${installed_version#v}-${operating_system}-${architecture}"
release_directory="${install_root}/releases/${release_name}"
if [ ! -d "$release_directory" ]; then
  staged_release="$(mktemp -d "${install_root}/releases/.install.XXXXXX")"
  mkdir "${staged_release}/bin"
  cp "$binary_path" "${staged_release}/bin/inline"
  chmod 0755 "${staged_release}/bin/inline"
  mv "$staged_release" "$release_directory"
  staged_release=""
fi
[ -x "${release_directory}/bin/inline" ] || fail "${release_directory} exists but is not a valid Inline release"

ln -sfn "releases/${release_name}" "${install_root}/current"
ln -sfn "${install_root}/current/bin/inline" "${bin_directory}/inline"

printf 'Installed Inline v%s at %s\n' "${installed_version#v}" "${bin_directory}/inline"
case ":${PATH}:" in
  *:"${bin_directory}":*) ;;
  *)
    printf '\nAdd Inline to PATH, then restart your shell:\n'
    printf '  export PATH="%s:$PATH"\n' "$bin_directory"
    ;;
esac
