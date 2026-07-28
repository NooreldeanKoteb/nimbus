#!/usr/bin/env sh
# Cross-compile nimbus as a static, dependency-free binary.
#
# CGO_ENABLED=0 is the load-bearing setting: it removes the libc dynamic link,
# which is what lets one binary run on Alpine, Ubuntu, and a scratch container
# alike (DESIGN.md §2a).
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT="$ROOT/dist"

VERSION=${VERSION:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}
COMMIT=${COMMIT:-$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo none)}
DATE=${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}

PKG=github.com/nkoteb/nimbus/internal/version
LDFLAGS="-s -w -X $PKG.Version=$VERSION -X $PKG.Commit=$COMMIT -X $PKG.Date=$DATE"

# -s -w strips the symbol table and DWARF data; nothing at runtime needs them.
TARGETS=${TARGETS:-"linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"}

rm -rf "$OUT"
mkdir -p "$OUT"

for target in $TARGETS; do
    goos=${target%/*}
    goarch=${target#*/}

    name="nimbus-$goos-$goarch"
    [ "$goos" = "windows" ] && name="$name.exe"

    printf '  %-24s' "$goos/$goarch"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/$name" "$ROOT/cmd/nimbus"

    size=$(wc -c < "$OUT/$name" | tr -d ' ')
    printf 'ok  %s KiB\n' "$((size / 1024))"
done

# Checksums let install.sh verify a download without extra tooling.
if command -v sha256sum >/dev/null 2>&1; then
    (cd "$OUT" && sha256sum ./* > SHA256SUMS)
elif command -v shasum >/dev/null 2>&1; then
    (cd "$OUT" && shasum -a 256 ./* > SHA256SUMS)
fi

echo
echo "built $VERSION -> $OUT"
