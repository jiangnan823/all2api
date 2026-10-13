#!/usr/bin/env bash
# Build an fnOS .fpk for all2api.
#
# Usage:
#   FPK_VERSION=0.1.0 FPK_IMAGE_TAG=v0.1.0 bash deploy/fnos/package-fpk.sh
#
# Requires:
#   - fnpack (飞牛应用打包工具) on PATH, or --emit-dir to stop at the staged tree
#   - icons at deploy/fnos/assets/icon_64.png and icon_256.png
#
# Output: deploy/fnos/build/all2api_<version>.fpk  (+ all2api_<version>_dir/ if --emit-dir)
#
# Design note: this script never invents fields silently. It renders the template,
# validates the invariants we know fnOS enforces (3-part version, arch match,
# pinned image tag), and then hands off to fnpack. Anything fnpack rejects is a
# signal to fix the template — not to paper over here.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

APP="all2api"
VERSION="${FPK_VERSION:?FPK_VERSION is required (must be X.Y.Z)}"
IMAGE_REPO="${FPK_IMAGE_REPO:-ghcr.io/jiangnan823/all2api}"
IMAGE_TAG="${FPK_IMAGE_TAG:?FPK_IMAGE_TAG is required — fpk must pin an immutable tag, not :latest}"
PLATFORM="${FPK_PLATFORM:-x86}"
OUT_DIR="$HERE/build"
STAGE="$OUT_DIR/${APP}_${VERSION}"

emit_dir=0
for a in "$@"; do [ "$a" = "--emit-dir" ] && emit_dir=1; done

fail() { echo "package-fpk: $*" >&2; exit 1; }

# ------------------------------------------------------------- invariants ----
echo "== validating inputs"
echo "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' \
  || fail "FPK_VERSION must be X.Y.Z (fnOS app center rejects 2-part versions); got '$VERSION'"
case "$PLATFORM" in x86|arm) ;; *) fail "FPK_PLATFORM must be x86 or arm, got '$PLATFORM'";; esac
[ "$IMAGE_TAG" != "latest" ] || fail "refusing to ship fpk pinned to :latest"
command -v docker >/dev/null 2>&1 || echo "WARN: docker not found; compose runtime path untested"

# ------------------------------------------------------------------ stage ----
echo "== staging $STAGE"
rm -rf "$STAGE"
mkdir -p "$STAGE"
cp -R "$HERE/template/." "$STAGE/"

render() {
  local f="$1"
  sed -i \
    -e "s|{{IMAGE_REPO}}|${IMAGE_REPO}|g" \
    -e "s|{{IMAGE_TAG}}|${IMAGE_TAG}|g" \
    "$f"
}

render "$STAGE/app/docker/docker-compose.yaml"

# manifest: version + platform + service_port are the three fields that must agree
# with the rest of the package. Everything else stays as authored.
sed -i \
  -e "s|^version=.*|version=\"${VERSION}\"|" \
  -e "s|^platform=.*|platform=\"${PLATFORM}\"|" \
  "$STAGE/manifest"

# compose 默认 config 与镜像仓库同步
sed -i "s|image:.*|image: \${ALL2API_IMAGE:-${IMAGE_REPO}}:\${ALL2API_VERSION:-${IMAGE_TAG}}|" \
  "$STAGE/app/docker/docker-compose.yaml" 2>/dev/null || true

chmod +x "$STAGE/cmd/main"

# icons ------------------------------------------------------------ icons ----
ASSETS="$HERE/assets"
for i in icon_64.png icon_256.png; do
  [ -f "$ASSETS/$i" ] || fail "missing $ASSETS/$i — fnOS app center will not render the tile"
done
mkdir -p "$STAGE/app/ui/images"
cp "$ASSETS/icon_64.png"  "$STAGE/app/ui/images/icon_64.png"
cp "$ASSETS/icon_256.png" "$STAGE/app/ui/images/icon_256.png"
# Some fnpack builds expect capital names at the package root as well.
cp "$ASSETS/icon_64.png"  "$STAGE/ICON.PNG"
cp "$ASSETS/icon_256.png" "$STAGE/ICON_256.PNG"

# ship the config example so cmd/main can seed on first install
cp "$ROOT/config.example.json" "$STAGE/app/docker/config.example.json"

# ------------------------------------------------------------------ checks ----
echo "== post-stage checks"
grep -q '{{IMAGE' "$STAGE/app/docker/docker-compose.yaml" \
  && fail "unrendered placeholder left in compose"
python3 - "$STAGE" <<'PY'
import json, sys, pathlib
stage = pathlib.Path(sys.argv[1])
for rel in ("config/privilege", "config/resource", "app/ui/config"):
    p = stage / rel
    try:
        json.loads(p.read_text(encoding="utf-8"))
    except Exception as e:
        print(f"FAIL {rel}: not valid JSON — {e}", file=sys.stderr)
        sys.exit(1)
print("json configs parse OK")
PY
bash -n "$STAGE/cmd/main" && echo "cmd/main syntax OK"
docker compose -f "$STAGE/app/docker/docker-compose.yaml" config >/dev/null 2>&1 \
  && echo "compose config OK" \
  || echo "WARN: docker compose config check unavailable here (run it on the NAS)"

# ------------------------------------------------------------------- build ----
if [ "$emit_dir" = 1 ]; then
  echo "== staged tree kept at $STAGE"
fi

if command -v fnpack >/dev/null 2>&1; then
  echo "== fnpack build"
  ( cd "$OUT_DIR" && fnpack build --directory "${APP}_${VERSION}" )
  # fnpack writes <name>.fpk; normalise our expected output path
  built="$(find "$OUT_DIR" -maxdepth 2 -name '*.fpk' -newermt '-5 minutes' | head -n1)"
  [ -n "$built" ] || fail "fnpack reported success but produced no .fpk"
  mv "$built" "$OUT_DIR/${APP}_${VERSION}.fpk"
  echo "== output: $OUT_DIR/${APP}_${VERSION}.fpk"
  sha256sum "$OUT_DIR/${APP}_${VERSION}.fpk" | tee "$OUT_DIR/${APP}_${VERSION}.fpk.sha256"
else
  echo "== fnpack not found — emitting tar.gz fallback for manual transfer"
  echo "   (NOT a valid .fpk; use only for offline template review. On the NAS:"
  echo "    fnpack build --directory ${APP}_${VERSION})"
  tar -C "$OUT_DIR" -czf "$OUT_DIR/${APP}_${VERSION}_staged.tar.gz" "${APP}_${VERSION}"
fi

echo
echo "== install on NAS (once a real .fpk exists):"
echo "   scp ${APP}_${VERSION}.fpk user@nas:/tmp/"
echo "   ssh nas 'sudo appcenter-cli install-fpk /tmp/${APP}_${VERSION}.fpk'"
echo "   or drag the file into 应用中心 → 手动安装"
