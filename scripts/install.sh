#!/usr/bin/env sh
# install.sh — install zot-warm from the universal archive.
#
# Usage:
#   sudo ./install.sh                # install to /usr/local
#   ./install.sh --prefix ~/.local   # user-level install
#   ./install.sh --help
#
# Lays out:
#   <prefix>/bin/zot-warm                      (selector script)
#   <prefix>/lib/zot-warm/zot-warm-linux-amd64
#   <prefix>/lib/zot-warm/zot-warm-linux-arm64

set -eu

prefix="/usr/local"

while [ $# -gt 0 ]; do
    case "$1" in
        --prefix)
            prefix="$2"
            shift 2
            ;;
        --prefix=*)
            prefix="${1#--prefix=}"
            shift
            ;;
        -h|--help)
            cat <<EOF
install.sh — install zot-warm from the universal archive.

Usage:
  install.sh [--prefix PATH]

Options:
  --prefix PATH   Installation prefix (default: /usr/local)
  -h, --help      Show this help and exit

The installer lays out:
  <prefix>/bin/zot-warm                      selector script (your entry point)
  <prefix>/lib/zot-warm/zot-warm-linux-amd64 arch-specific binary
  <prefix>/lib/zot-warm/zot-warm-linux-arm64 arch-specific binary

Run the resulting \`zot-warm\` from PATH — it detects arch and execs the
right binary from <prefix>/lib/zot-warm.
EOF
            exit 0
            ;;
        *)
            printf 'install.sh: unknown option %s\n' "$1" >&2
            exit 2
            ;;
    esac
done

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
bin_dir="${prefix}/bin"
lib_dir="${prefix}/lib/zot-warm"

for required in zot-warm zot-warm-linux-amd64 zot-warm-linux-arm64; do
    if [ ! -f "${script_dir}/${required}" ]; then
        printf 'install.sh: missing %s in archive\n' "$required" >&2
        exit 1
    fi
done

mkdir -p "$bin_dir" "$lib_dir"
cp "${script_dir}/zot-warm-linux-amd64" "${lib_dir}/"
cp "${script_dir}/zot-warm-linux-arm64" "${lib_dir}/"
chmod 755 "${lib_dir}/zot-warm-linux-amd64" "${lib_dir}/zot-warm-linux-arm64"

cp "${script_dir}/zot-warm" "${bin_dir}/zot-warm"
chmod 755 "${bin_dir}/zot-warm"

printf 'Installed zot-warm to %s\n' "${bin_dir}/zot-warm"
printf 'Architecture-specific binaries in %s\n' "${lib_dir}"
printf '\n'
printf 'Verify: %s --version\n' "${bin_dir}/zot-warm"
