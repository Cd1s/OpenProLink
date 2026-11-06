#!/bin/sh
# Refreshes third_party/enginex from an EngineX OS checkout: the receiver,
# exporter and library code OpenProLink tests against. Copies sources only (no
# tests) and rewrites import paths; never edit the copy, fix it upstream.
#   sh scripts/sync-enginex.sh <path to EngineX OS checkout>
set -eu
SRC=${1:?path to an EngineX OS checkout}
DST=third_party/enginex
OLD=github.com/Cd1s/EngineX-OS
NEW=github.com/Cd1s/OpenProLink/third_party/enginex
[ -f "$SRC/go.mod" ] && grep -q "^module $OLD\$" "$SRC/go.mod" || { echo "$SRC is not EngineX OS" >&2; exit 2; }
rm -rf "$DST"
for pkg in pkg/prolink pkg/rekordbox pkg/compat pkg/enginedb pkg/bridge internal/testfixture; do
    out=$DST/${pkg##*/}  # flat: an internal/ path would hide testfixture from cmd/
    mkdir -p "$out"
    for f in "$SRC/$pkg"/*.go; do
        case $f in *_test.go) continue ;; esac
        sed -e "s#\"$OLD/pkg/#\"$NEW/#g" -e "s#\"$OLD/internal/#\"$NEW/#g" "$f" > "$out/${f##*/}"
    done
    for f in "$SRC/$pkg"/*.sql; do [ ! -f "$f" ] || cp "$f" "$out/"; done  # go:embed
done
cp "$SRC/LICENSE" "$DST/LICENSE"
rev=$(git -C "$SRC" rev-parse --short HEAD 2>/dev/null || echo unknown)
dirty=$(git -C "$SRC" status --porcelain -- pkg internal 2>/dev/null | wc -l | tr -d ' ')
printf 'EngineX OS %s (%s uncommitted files under pkg/ and internal/), synced %s\n' "$rev" "$dirty" "$(date -u +%Y-%m-%dT%H:%MZ)" > "$DST/VERSION"
echo "synced: $(cat "$DST/VERSION")"
