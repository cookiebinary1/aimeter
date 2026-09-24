#!/bin/sh
# Install a verified release binary. Keep POSIX sh compatibility for curl | sh.
set -eu

fail() {
    printf 'aimeter installer: %s\n' "$*" >&2
    exit 1
}

fetch() {
    curl --fail --silent --show-error --location \
        --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --connect-timeout 15 --max-time 180 --retry 2 "$@"
}

cleanup() {
    [ -z "${staged_binary:-}" ] || rm -f "$staged_binary"
    [ -z "${work_dir:-}" ] || rm -rf "$work_dir"
}

main() {
    case "${1:-}" in
        -h|--help)
            printf '%s\n' 'Install aimeter for macOS/Linux (amd64/arm64).' \
                'Usage: sh install.sh' \
                'AIMETER_VERSION=v0.1.1       Pin a release (default: latest).' \
                'AIMETER_INSTALL_DIR=/path   Install directory (default: $HOME/.local/bin).'
            return
            ;;
        '') ;;
        *) fail "Unknown argument: $1. Use --help." ;;
    esac
    [ "$#" -le 1 ] || fail 'Too many arguments. Use --help.'

    case "$(uname -s)" in
        Darwin) os=darwin ;;
        Linux) os=linux ;;
        *) fail 'Supported systems: macOS and Linux. On Windows, use Go or a ZIP from GitHub Releases.' ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        arm64|aarch64) arch=arm64 ;;
        *) fail 'Supported architectures: amd64 and arm64.' ;;
    esac
    for tool in curl tar awk mktemp mkdir cp chmod mv rm; do
        command -v "$tool" >/dev/null 2>&1 || fail "Required command not found: $tool"
    done
    if command -v sha256sum >/dev/null 2>&1; then
        checksum_tool=sha256sum
    elif command -v shasum >/dev/null 2>&1; then
        checksum_tool=shasum
    else
        fail 'Install sha256sum or shasum to verify the download.'
    fi

    install_dir=${AIMETER_INSTALL_DIR:-${HOME:?HOME is required}/.local/bin}
    case "$install_dir" in
        /*) ;;
        *) fail 'AIMETER_INSTALL_DIR must be an absolute path.' ;;
    esac
    release_base=https://github.com/cookiebinary1/aimeter/releases
    version=${AIMETER_VERSION:-latest}
    if [ "$version" = latest ]; then
        release_url=$(fetch --output /dev/null --write-out '%{url_effective}' "$release_base/latest") || fail 'Could not resolve the latest release.'
        case "$release_url" in
            "$release_base"/tag/v*) version=${release_url##*/} ;;
            *) fail 'Unexpected latest-release URL.' ;;
        esac
    fi
    version=${version#v}
    case "$version" in
        ''|*[!0-9A-Za-z.+-]*) fail 'Invalid release version.' ;;
    esac
    case "$version" in
        [0-9]*) ;;
        *) fail 'Release versions must start with a digit.' ;;
    esac
    archive="aimeter_${version}_${os}_${arch}.tar.gz"
    download_base="$release_base/download/v$version"

    work_dir=$(mktemp -d "${TMPDIR:-/tmp}/aimeter-install.XXXXXXXX") || fail 'Could not create temporary directory.'
    staged_binary=
    trap cleanup EXIT
    trap 'exit 1' HUP INT TERM
    printf 'Downloading aimeter v%s for %s/%s...\n' "$version" "$os" "$arch"
    fetch --output "$work_dir/$archive" "$download_base/$archive" || fail 'Could not download the release archive.'
    fetch --output "$work_dir/checksums.txt" "$download_base/checksums.txt" || fail 'Could not download release checksums.'
    expected=$(awk -v name="$archive" '$2 == name || $2 == "*" name { print $1 }' "$work_dir/checksums.txt")
    [ "${#expected}" -eq 64 ] || fail 'Missing or ambiguous SHA-256 checksum for this archive.'
    case "$expected" in
        *[!0-9a-f]*) fail 'Invalid SHA-256 checksum.' ;;
    esac
    if [ "$checksum_tool" = sha256sum ]; then
        checksum_output=$(sha256sum "$work_dir/$archive") || fail 'Could not calculate SHA-256.'
    else
        checksum_output=$(shasum -a 256 "$work_dir/$archive") || fail 'Could not calculate SHA-256.'
    fi
    actual=${checksum_output%% *}
    [ "$actual" = "$expected" ] || fail 'SHA-256 mismatch. Nothing was installed.'

    # Extract only the binary; never unpack arbitrary archive paths into the install directory.
    tar -xzf "$work_dir/$archive" -C "$work_dir" aimeter || fail 'Could not extract aimeter.'
    [ -f "$work_dir/aimeter" ] && [ ! -L "$work_dir/aimeter" ] || fail 'The archive does not contain a regular aimeter binary.'
    mkdir -p "$install_dir" || fail 'Could not create install directory. Choose a writable AIMETER_INSTALL_DIR.'
    [ ! -d "$install_dir/aimeter" ] || fail 'The destination aimeter is a directory.'
    # Stage on the same filesystem and replace only after every check succeeds.
    staged_binary=$(mktemp "$install_dir/.aimeter.XXXXXXXX") || fail 'Install directory is not writable.'
    cp "$work_dir/aimeter" "$staged_binary"
    chmod 755 "$staged_binary"
    mv -f "$staged_binary" "$install_dir/aimeter"
    staged_binary=
    printf 'Installed aimeter v%s to %s/aimeter\n' "$version" "$install_dir"
    case ":${PATH:-}:" in
        *":$install_dir:"*) printf '%s\n' 'Run: aimeter' ;;
        *) printf 'Add %s to your PATH, or run %s/aimeter directly.\n' "$install_dir" "$install_dir" ;;
    esac
}

# Put execution last so an incomplete download cannot run a partial installer.
main "$@"
