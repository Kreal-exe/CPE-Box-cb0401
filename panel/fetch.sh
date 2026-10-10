#!/bin/bash
#
# fetch.sh - gets the CPE Box binaries, so running CPE Box doesn't need Go.
# Sourced by start_gui.sh and setup.sh.
#
#   ensure_gui_bin      -> panel/cpe-box, built from source when Go is installed
#                          (so it always matches this checkout), otherwise the
#                          latest GitHub release (re-downloaded when a newer
#                          one is out).
#   fetch_sms_reader D  -> D/sms-reader for the router (ARMv7), same rule.
#
# CPEBOX_NO_BUILD=1 skips building even with Go installed.

CPEBOX_REPO="${CPEBOX_REPO:-Kreal-exe/CPE-Box-cb0401}"
_FETCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Go 1.21+ is what the code targets (embed, min/max, ...). Older versions
# fail at "go build" with unhelpful errors like "undefined: min", so we
# just skip the build path and use a prebuilt release instead.
_have_go() {
  [ -n "${CPEBOX_NO_BUILD:-}" ] && return 1
  command -v go >/dev/null 2>&1 || return 1
  local v
  v="$(go env GOVERSION 2>/dev/null | sed -n 's/^go//p')"
  case "$v" in
    "" | 1.[0-9] | 1.[0-9].* | 1.1[0-9] | 1.1[0-9].* | 1.20 | 1.20.*) return 1 ;;
  esac
  return 0
}

_latest_tag() {
  curl -fsSL --max-time 15 "https://api.github.com/repos/$CPEBOX_REPO/releases/latest" 2>/dev/null |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1
}

# _download TAG ASSET DEST
_download() {
  local tmp="$3.download"
  if curl -fL --max-time 300 --progress-bar -o "$tmp" "https://github.com/$CPEBOX_REPO/releases/download/$1/$2"; then
    chmod +x "$tmp" && mv -f "$tmp" "$3"
  else
    rm -f "$tmp"
    return 1
  fi
}

_gui_asset() {
  case "$(uname -s)/$(uname -m)" in
    Darwin/arm64) echo cpe-box-macos-arm64 ;;
    Darwin/x86_64) echo cpe-box-macos-intel ;;
    Linux/x86_64) echo cpe-box-linux-amd64 ;;
    Linux/aarch64 | Linux/arm64) echo cpe-box-linux-arm64 ;;
    Linux/armv7l | Linux/armv8l | Linux/armv6l | Linux/arm) echo cpe-box-linux-armv7 ;;
    *) return 1 ;;
  esac
}

ensure_gui_bin() {
  local bin="$_FETCH_DIR/cpe-box" stamp="$_FETCH_DIR/.release"
  if _have_go; then
    echo "Building CPE Box from source..."
    # Stamp the version the same way build.sh does for releases (from the git
    # tag, e.g. 1.0.10, or 1.0.10-3-gabc1234 past it), so the panel doesn't
    # show the placeholder version from the source.
    local ver
    ver="$(git -C "$_FETCH_DIR" describe --tags --always 2>/dev/null | sed 's/^v//')"
    (cd "$_FETCH_DIR" && go build -ldflags="-X main.appVersion=${ver:-dev}" -o "$bin" .) && return 0
    echo "Build failed - trying a prebuilt release instead."
  fi
  local asset tag
  asset="$(_gui_asset)" || {
    [ -x "$bin" ] && return 0
    echo "ERROR: no prebuilt CPE Box for $(uname -s)/$(uname -m); install Go (https://go.dev/dl/) and re-run." >&2
    return 1
  }
  tag="$(_latest_tag)"
  if [ -z "$tag" ]; then
    [ -x "$bin" ] && { echo "Couldn't check GitHub for updates - using the CPE Box already here."; return 0; }
    echo "ERROR: couldn't reach GitHub to download CPE Box (and Go isn't installed to build it)." >&2
    return 1
  fi
  if [ -x "$bin" ] && [ "$(cat "$stamp" 2>/dev/null)" = "$tag" ]; then
    return 0
  fi
  echo "Downloading CPE Box $tag..."
  if _download "$tag" "$asset" "$bin"; then
    echo "$tag" > "$stamp"
    return 0
  fi
  [ -x "$bin" ] && { echo "Download failed - using the CPE Box already here."; return 0; }
  echo "ERROR: downloading $asset from $tag failed." >&2
  return 1
}

# fetch_sms_reader DIR - leaves DIR/sms-reader (ARMv7 static binary), or
# returns 1. First tries dumping it out of the already-fetched cpe-box (it
# ships embedded, see panel/embed_smsreader.go); falls back to a fresh
# cross-build if Go is present, and finally to a leftover dist/ copy.
fetch_sms_reader() {
  local out="$1/sms-reader" src="$_FETCH_DIR/../router/sms-reader" bin="$_FETCH_DIR/cpe-box"
  if [ -x "$bin" ] && "$bin" --dump-sms-reader "$out" 2>/dev/null; then
    return 0
  fi
  if _have_go; then
    (cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o "$out" .) && return 0
  fi
  [ -x "$src/dist/sms-reader-arm" ] && cp "$src/dist/sms-reader-arm" "$out" && return 0
  return 1
}
