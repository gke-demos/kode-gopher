#!/usr/bin/env bash
# Materializes a patched yaegi into third_party/yaegi (gitignored), which
# go.mod's replace directive points at. Run once after cloning, and again
# whenever a patch in patches/ changes.
#
# Patches carried (see docs/decisions.md for each):
#   yaegi-v0.16.1-named-results.patch — named results were not reset between
#     calls from the same call site, silently returning wrong values.
set -euo pipefail
cd "$(dirname "$0")"

VERSION=v0.16.1
# Outside this module: its replace directive may point at a directory that
# doesn't exist yet.
(cd / && go mod download "github.com/traefik/yaegi@${VERSION}")
SRC="$(go env GOMODCACHE)/github.com/traefik/yaegi@${VERSION}"

rm -rf third_party/yaegi
mkdir -p third_party
cp -r "$SRC" third_party/yaegi
chmod -R u+w third_party/yaegi
# git apply rather than patch(1), which isn't always installed. Run from the
# target dir: inside a checkout git resolves patch paths relative to cwd;
# outside one it behaves like patch -p1.
for p in patches/yaegi-${VERSION}-*.patch; do
  (cd third_party/yaegi && git apply -p1 "../../$p")
done
echo "third_party/yaegi: yaegi ${VERSION} + $(ls patches/yaegi-${VERSION}-*.patch | wc -l) patch(es)"
