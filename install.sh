#!/usr/bin/env bash
# install.sh — Install the latest enola release.
# Usage: curl -fsSL https://raw.githubusercontent.com/enola-labs/enola/main/install.sh | sh

set -euo pipefail

# --- Detect OS ---
OS="$(uname -s)"
case "$OS" in
  Linux)   OS=linux ;;
  Darwin)  OS=darwin ;;
  *)       echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

# --- Detect arch ---
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)       ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *)            echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

# --- Fetch latest version ---
VERSION="$(curl -fsSL https://api.github.com/repos/enola-labs/enola/releases/latest | grep '"tag_name"' | sed -E 's/.*"v([^"]+)".*/\1/')"
if [ -z "$VERSION" ]; then
  echo "Failed to fetch latest version" >&2
  exit 1
fi

BASE="enola-${VERSION}-${OS}-${ARCH}"
ASSET="${BASE}.tar.gz"
SHASUM="${BASE}.sha256"
URL="https://github.com/enola-labs/enola/releases/download/v${VERSION}/${ASSET}"
SUM_URL="https://github.com/enola-labs/enola/releases/download/v${VERSION}/${SHASUM}"

echo "==> Downloading enola v${VERSION} for ${OS}/${ARCH} ..."

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

curl -fsSL -o "$TMPDIR/$ASSET" "$URL"
curl -fsSL -o "$TMPDIR/$SHASUM" "$SUM_URL"

echo "==> Verifying checksum ..."
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$TMPDIR" && sha256sum -c "$SHASUM")
else
  (cd "$TMPDIR" && shasum -a 256 -c "$SHASUM")
fi

echo "==> Extracting ..."
tar xzf "$TMPDIR/$ASSET" -C "$TMPDIR"

# --- Install ---
BIN_NAME="${BASE}"
if [ "$OS" = "windows" ]; then
  BIN_NAME="${BIN_NAME}.exe"
fi

INSTALL_DIR="${ENOLA_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"

cp "$TMPDIR/$BIN_NAME" "$INSTALL_DIR/enola"

echo "==> enola v${VERSION} installed to $INSTALL_DIR/enola"
echo ""
echo "If \$HOME/.local/bin is not in your PATH, add it:"
echo "  export PATH=\"$HOME/.local/bin:\$PATH\""
