#!/bin/sh
# SessionStart bootstrap for the bashback Claude Code plugin.
#
# A freshly installed plugin has no bashback binary on PATH. This script
# downloads the release binary once into the plugin's persistent data dir
# (CLAUDE_PLUGIN_DATA/bin/bashback) so the hook commands can resolve it.
#
# Hard rule: fail open. This runs on every SessionStart and must NEVER block
# or fail a Claude session. Every error path exits 0. No blanket `set -e`.

# Base repo for release assets. Hardcoded: the download origin must not be
# overridable by the environment, or the checksum defense is moot.
BASE="https://github.com/trouties/bashback"

DATA="${CLAUDE_PLUGIN_DATA:-}"
BIN="${DATA}/bin/bashback"

# Best-effort debug log under the data dir; never writes to stderr.
log() {
  [ -n "$DATA" ] || return 0
  { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)" "$1" \
    >>"${DATA}/bootstrap.log"; } 2>/dev/null || true
}

# The skill's bare `bashback ...` commands resolve through the shipped
# bin/bashback shim on ${CLAUDE_PLUGIN_ROOT}/bin (tracked plugin content); this
# bootstrap only fetches the real binary into the data dir and never writes to
# the plugin root.

# Without a data dir there is nowhere stable to put the binary; fall back to
# whatever PATH provides and leave the session untouched.
[ -n "$DATA" ] || { log "no CLAUDE_PLUGIN_DATA; skip"; exit 0; }

# Skip path: binary already bootstrapped. No network.
[ -x "$BIN" ] && { log "skip: binary already present"; exit 0; }

# Need curl and tar to proceed.
command -v curl >/dev/null 2>&1 || { log "no curl; skip"; exit 0; }
command -v tar  >/dev/null 2>&1 || { log "no tar; skip"; exit 0; }

# Detect os/arch using the same mapping as goreleaser asset names.
os=$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')
arch=$(uname -m 2>/dev/null)
case "$arch" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) log "unsupported arch: ${arch:-unknown}; skip"; exit 0 ;;
esac
case "$os" in
  linux|darwin) ;;
  *) log "unsupported os: ${os:-unknown}; skip"; exit 0 ;;
esac

# Release assets are versioned (bashback_<version>_<os>_<arch>.tar.gz); there is
# no version-less "latest" asset, so resolve the tag from the releases/latest
# redirect and build the versioned URL.
tag=$(curl -fsSL -o /dev/null -w '%{url_effective}' \
  "${BASE}/releases/latest" 2>/dev/null)
tag=${tag##*/}
case "$tag" in
  v[0-9]*[!A-Za-z0-9.+_-]*) log "unexpected tag: $tag; skip"; exit 0 ;;
  v[0-9]*) ;;
  *) log "could not resolve tag (got: '${tag:-}'); skip"; exit 0 ;;
esac
version=${tag#v}

asset="bashback_${version}_${os}_${arch}.tar.gz"
url="${BASE}/releases/download/${tag}/${asset}"
sums_url="${BASE}/releases/download/${tag}/checksums.txt"

# Temp files with an EXIT cleanup trap; never a fixed /tmp path.
# BSD mktemp (macOS) requires the X's at the very end of the template; a
# trailing suffix makes it fail with "too few X's". The random component alone
# disambiguates the two files, and the extension is irrelevant to tar/checksum.
tmp=$(mktemp "${TMPDIR:-/tmp}/bashback.XXXXXX" 2>/dev/null) || {
  log "mktemp failed; skip"; exit 0; }
sums=$(mktemp "${TMPDIR:-/tmp}/bashback.XXXXXX" 2>/dev/null) || {
  rm -f "$tmp"; log "mktemp failed; skip"; exit 0; }
stage=""
trap 'rm -f "$tmp" "$sums" "$stage"' EXIT

# Download asset and checksums.
curl -fsSL "$url" -o "$tmp" 2>/dev/null || {
  log "download failed: $url; skip"; exit 0; }
curl -fsSL "$sums_url" -o "$sums" 2>/dev/null || {
  log "checksums download failed; skip"; exit 0; }

# Find the checksum line for this exact asset and verify it. Match the filename
# field literally (a glob/regex would treat the dots in the name as wildcards).
expected=""
while read -r h f; do
  [ "$f" = "$asset" ] && { expected=$h; break; }
done < "$sums"
[ -n "$expected" ] || { log "no checksum for $asset; skip"; exit 0; }

if command -v sha256sum >/dev/null 2>&1; then
  printf '%s  %s\n' "$expected" "$tmp" | sha256sum -c - >/dev/null 2>&1 || {
    log "checksum mismatch for $asset; skip"; exit 0; }
elif command -v shasum >/dev/null 2>&1; then
  printf '%s  %s\n' "$expected" "$tmp" | shasum -a 256 -c - >/dev/null 2>&1 || {
    log "checksum mismatch for $asset; skip"; exit 0; }
else
  log "no sha256 tool; skip"; exit 0
fi

# Install atomically: stream the single named member to a private staging file
# in the SAME directory, then mv it into place (an atomic same-filesystem
# rename). This avoids two concurrent sessions writing the final path at once,
# and avoids a truncated binary becoming sticky (the skip check trusts -x).
mkdir -p "${DATA}/bin" 2>/dev/null || { log "mkdir failed; skip"; exit 0; }
stage="${DATA}/bin/.bashback.$$.tmp"
tar -xzOf "$tmp" bashback > "$stage" 2>/dev/null || {
  rm -f "$stage"; log "extract failed; skip"; exit 0; }
chmod +x "$stage" 2>/dev/null || true
mv -f "$stage" "$BIN" 2>/dev/null || {
  rm -f "$stage"; log "install failed; skip"; exit 0; }

log "ok: $url"
exit 0
