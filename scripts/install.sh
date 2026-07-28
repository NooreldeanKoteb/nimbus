#!/usr/bin/env sh
# Nimbus installer.
#
#   curl -fsSL <installer-url> | sh
#
# No installer URL is published yet: nimbus.sh is not a domain this project
# owns, and pointing the one-liner at a host somebody else controls would be
# telling users to pipe a stranger's server into a shell. Until a release
# exists, build from source (see README) — this script already works, it just
# has nothing to fetch.
#
# POSIX sh only, and depends on nothing but curl-or-wget and uname. This runs on
# a device with nothing installed, so it cannot assume bash, git, or a package
# manager (DESIGN.md §2a).
set -eu

REPO=${NIMBUS_REPO:-nkoteb/nimbus}
VERSION=${NIMBUS_VERSION:-latest}
PREFIX=${NIMBUS_PREFIX:-$HOME/.local}
BINDIR="$PREFIX/bin"

die() { printf 'error: %s\n' "$1" >&2; exit 1; }

detect_os() {
    case $(uname -s) in
        Linux)   echo linux ;;
        Darwin)  echo darwin ;;
        MINGW*|MSYS*|CYGWIN*) echo windows ;;
        *) die "unsupported OS: $(uname -s)" ;;
    esac
}

detect_arch() {
    case $(uname -m) in
        x86_64|amd64)  echo amd64 ;;
        arm64|aarch64) echo arm64 ;;
        *) die "unsupported architecture: $(uname -m)" ;;
    esac
}

# Prefer curl, fall back to wget; one of the two is present on essentially
# every system that can reach the internet at all.
download() {
    url=$1; dest=$2
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$dest" || die "download failed: $url"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "$dest" "$url" || die "download failed: $url"
    else
        die "need curl or wget to install"
    fi
}

main() {
    os=$(detect_os)
    arch=$(detect_arch)

    name="nimbus-$os-$arch"
    [ "$os" = "windows" ] && name="$name.exe"

    if [ "$VERSION" = "latest" ]; then
        url="https://github.com/$REPO/releases/latest/download/$name"
    else
        url="https://github.com/$REPO/releases/download/$VERSION/$name"
    fi

    printf 'installing nimbus (%s/%s)\n' "$os" "$arch"

    tmp=$(mktemp -d 2>/dev/null || mktemp -d -t nimbus)
    trap 'rm -rf "$tmp"' EXIT INT TERM

    download "$url" "$tmp/nimbus"
    chmod +x "$tmp/nimbus"

    # Run before installing: a binary that cannot execute here is worse than
    # no binary, and this catches wrong-arch downloads immediately.
    "$tmp/nimbus" version >/dev/null 2>&1 || die "downloaded binary failed to run"

    mkdir -p "$BINDIR"
    mv "$tmp/nimbus" "$BINDIR/nimbus"

    printf 'installed to %s\n' "$BINDIR/nimbus"

    case ":$PATH:" in
        *":$BINDIR:"*) ;;
        *)
            printf '\n%s is not on your PATH. Add it with:\n' "$BINDIR"
            printf '  echo '\''export PATH="%s:$PATH"'\'' >> ~/.profile\n' "$BINDIR"
            ;;
    esac

    printf '\nnext:  nimbus login\n'
}

main "$@"
