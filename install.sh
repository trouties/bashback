#!/bin/sh
# bashback installer: download the release binary, then wire the requested
# platform (claude|codex|cursor). Usage: ./install.sh [platform]
set -eu

PLATFORM="${1:-claude}"
case "$PLATFORM" in
  claude|codex|cursor) ;;
  *) echo "unknown platform: $PLATFORM (use claude|codex|cursor)" >&2; exit 2;;
esac

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64) arch=amd64;;
  aarch64|arm64) arch=arm64;;
  *) echo "unsupported arch: $arch" >&2; exit 1;;
esac
case "$os" in
  linux|darwin) ;;
  *) echo "unsupported os: $os (use WSL2 on Windows)" >&2; exit 1;;
esac

REPO="trouties/bashback"

# Release assets are versioned (bashback_<version>_<os>_<arch>.tar.gz) and there
# is no version-less "latest" asset. Pin a release with BASHBACK_VERSION=v1.0.0;
# otherwise resolve the latest tag from the redirect GitHub serves for
# releases/latest, then build the versioned URL.
if [ -n "${BASHBACK_VERSION:-}" ]; then
  tag="$BASHBACK_VERSION"
else
  effective=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/${REPO}/releases/latest")
  tag=$(printf '%s\n' "$effective" | sed -n 's#.*/tag/##p')
fi
# The tag check is identical for the override and the resolved tag: the supply
# chain rule does not relax just because the version was pinned by hand.
case "$tag" in
  v[0-9]*[!A-Za-z0-9.+_-]*) echo "unexpected release tag: $tag" >&2; exit 1;;
  v[0-9]*) ;;
  *) echo "could not resolve release tag (got: '${tag:-}')" >&2; exit 1;;
esac
version=${tag#v}

asset="bashback_${version}_${os}_${arch}.tar.gz"
url="https://github.com/${REPO}/releases/download/${tag}/${asset}"
sums_url="https://github.com/${REPO}/releases/download/${tag}/checksums.txt"

BIN_DIR="${BASHBACK_BIN_DIR:-$HOME/.local/bin}"
mkdir -p "$BIN_DIR"

echo "downloading $url"
# BSD mktemp (macOS) needs the X's at the very end of the template; no suffix.
tmp=$(mktemp "${TMPDIR:-/tmp}/bashback.XXXXXX")
sums=$(mktemp "${TMPDIR:-/tmp}/bashback.XXXXXX")
trap 'rm -f "$tmp" "$sums"' EXIT
curl -fsSL "$url" -o "$tmp"
curl -fsSL "$sums_url" -o "$sums"

# Verify the download against the published checksum before trusting the binary.
expected=""
while read -r h f; do
  [ "$f" = "$asset" ] && { expected=$h; break; }
done < "$sums"
[ -n "$expected" ] || { echo "no checksum for $asset" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  printf '%s  %s\n' "$expected" "$tmp" | sha256sum -c - >/dev/null 2>&1 \
    || { echo "checksum mismatch for $asset" >&2; exit 1; }
elif command -v shasum >/dev/null 2>&1; then
  printf '%s  %s\n' "$expected" "$tmp" | shasum -a 256 -c - >/dev/null 2>&1 \
    || { echo "checksum mismatch for $asset" >&2; exit 1; }
else
  echo "no sha256 tool (sha256sum/shasum) to verify download" >&2; exit 1
fi

tar -xzf "$tmp" -C "$BIN_DIR" bashback
chmod +x "$BIN_DIR/bashback"
echo "installed $BIN_DIR/bashback"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "note: $BIN_DIR is not on your PATH; add it so 'bashback' resolves" >&2 ;;
esac

case "$PLATFORM" in
  claude) "$BIN_DIR/bashback" install ;;
  codex)  "$BIN_DIR/bashback" install --codex ;;
  cursor) "$BIN_DIR/bashback" install --cursor ;;
esac

"$BIN_DIR/bashback" doctor || true
