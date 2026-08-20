#!/bin/sh
# hive installer — curl -fsSL https://hive.butttons.dev/setup.sh | bash
set -eu

REPO="butttons/hive"
BASE="https://github.com/$REPO/releases/latest/download"
DEST="${HIVE_INSTALL_DIR:-$HOME/.local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64)  arch=amd64 ;;
  *) echo "hive: unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin|linux) ;;
  *) echo "hive: unsupported OS: $os" >&2; exit 1 ;;
esac

asset="hive-$os-$arch"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "hive: downloading $asset (latest release)"
curl -fsSL "$BASE/$asset" -o "$tmp/$asset"
curl -fsSL "$BASE/checksums.txt" -o "$tmp/checksums.txt"

echo "hive: verifying checksum"
expected=$(grep "  $asset\$" "$tmp/checksums.txt" | awk '{print $1}')
if [ -z "$expected" ]; then
  echo "hive: no checksum found for $asset" >&2; exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
fi
if [ "$actual" != "$expected" ]; then
  echo "hive: checksum mismatch for $asset" >&2; exit 1
fi

mkdir -p "$DEST"
mv "$tmp/$asset" "$DEST/hive"
chmod +x "$DEST/hive"
echo "hive: installed to $DEST/hive"

case ":$PATH:" in
  *":$DEST:"*) ;;
  *) echo "hive: note — $DEST is not on your PATH" ;;
esac

cat <<'EOF'

next steps:
  1. install celld:  curl -fsSL https://celld.dev/install.sh | sh
  2. install esbuild (needed for deploys)
  3. hive add myapp && cd myapp && hive init
EOF
